# CLAUDE.md

## Project: slaptain (Kubernetes OpenLDAP Operator)

**Repository:** [github.com/chuck-chuck-chuck-net/slaptain](https://github.com/chuck-chuck-chuck-net/slaptain)

### Objective
Create a Kubernetes operator for a multi-master replicating OpenLDAP (slapd) cluster.
Decision drivers: functionality, performance, scalability first. Security hardening (rootless,
distroless, read-only root FS) as secondary goal — pursued where it doesn't compromise the primary drivers.

### Requirements & Constraints
- **Image:** Rootless, distroless. Debian/glibc base (not Alpine/musl — glibc required for
  production performance and compatibility at scale).
- **Architecture:**
    - **Multi-Site:** Setup spans multiple physical sites.
    - **Per-Site HA:** Each site must be autonomous and highly available internally (local K8s cluster).
    - **Cross-Cluster Replication:** Data must replicate between independent K8s clusters.
- **Platform:** Kubernetes (separate clusters per site).
- **Security:** Rootless execution (UID/GID 1024), no privilege escalation, read-only root FS.

---

### Implementation Status

- [x] slapd runtime image: Debian trixie-slim build → `gcr.io/distroless/base-debian13` (`images/slapd/Containerfile`).
- [x] slapd-init image: Debian trixie-slim, full shell environment for bootstrap (`images/slapd-init/Containerfile`).
- [x] Bootstrap logic with `slaptest` conversion (`images/slapd-init/bootstrap.sh`).
- [x] ~~Helm Chart for standalone deployment~~ — **removed in 0.2.0.** The pre-operator `charts/slapd` spoke the pre-ADR-004 init contract (`LDAP_DOMAIN_DC`, `LDAP_ADMIN_PW`, `/ldap-config`, `/ldap-data`, no `DATABASE_DIRS`) and bootstrap.sh reads none of it, so it produced a slapd with `cn=config` and no data database at all. `charts/slapd` is now the SlapdCluster chart (formerly `slapd-cluster`). An operator-free single slapd would be a new feature: a standalone data-DB bootstrap plus e2e coverage.
- [x] **Kubernetes Operator — Phase 1** (`operator/`): standalone single-replica StatefulSet managed by a kubebuilder controller. e2e: 33/33 green.
- [x] **Operator Phase 2**: N-way multi-master delta-syncrepl; operator-orchestrated bootstrap; per-pod `volumeClaimTemplates`; replication credential management. e2e: pending.
- [x] **Operator Phase 3**: cross-cluster replication via `ExternalPeers`. Operator owns all syncrepl configuration (in-cluster + external). See ADR-003. **TLS posture, stated precisely (verified 2026-09-14):** consumers verify the provider against a distributed CA (`tls_cacert`), relaxed to `tls_reqcert=allow` for IP-addressed peers (ADR-007). It is NOT mutual: `olcTLSVerifyClient` is never set, so slapd never requests a client certificate — the `tls_cert`/`tls_key` presented on external-peer stanzas are not verified by anything. Peer *authentication* is the simple bind, not the certificate. Cert-based peer auth (SASL EXTERNAL) is considered and deferred in ADR-027.
- [x] **S3 backup/restore** (ADR-014): `SlapdBackup` (on-demand) + `SlapdScheduledBackup` (cron + retention) → gzipped `slapcat` LDIF to S3 via co-located Jobs; `SlapdDatabase.spec.bootstrapFrom` restores into a fresh DB via a cluster-coordinated scale-to-0 → offline `slapadd` → scale-up machine. e2e green on t3e (versitygw S3 target). See `docs/BACKUP.md`.

---

### Repository Layout

```
.
├── Makefile                        # Root build targets (see Makefile Targets below)
├── lab.yaml.sample                 # Lab config schema for tests/e2e.sh (real lab.yaml is gitignored)
├── CLAUDE.md
├── docs/
│   ├── BOOTSTRAP.md                # Cluster bootstrap internals (init container + operator phases)
│   ├── DEVELOPMENT.md              # Dev guide: prerequisites, builds, operator loop, e2e cycle, debugging
│   ├── ONBOARDING.md               # Team onboarding: LDAP concepts, operator model, credential model
│   ├── MIGRATION-PLAN.md           # Phased plan for replacing a legacy OpenLDAP with slaptain
│   ├── MIGRATION-LEGACY-SOURCE.md  # Source-side (legacy slapd) prep for hot migration
│   ├── BACKUP.md                   # S3 backup/restore user guide (ADR-014)
│   ├── TUNING.md                   # Tuning & sizing guide: defaults, placement classes, lab→prod sizing (ADR-024)
│   ├── OPENLDAP-VERSIONS.md        # Dual 2.7/2.6 image pairs, tag scheme, 2.6→2.7 migration runbook (ADR-021)
│   ├── BACKUP-PLAN.md              # ADR-014 implementation breakdown (phases)
│   ├── MULTI-SITE.md               # USER guide: the mesh model, the invariants, a multi-site install (ADR-028)
│   ├── MESH-PLAN.md                # ADR-028 implementation breakdown (7 phases, all landed)
│   ├── BACKLOG.md                  # Cross-cutting tech debt (e.g. lint debt, e2e framework gaps)
│   └── adrs/
│       ├── adr-001-double-reconcile-runs.md
│       ├── adr-002-cn-config-node-local-operator-managed.md
│       ├── adr-003-operator-owns-syncrepl.md
│       ├── adr-004-multi-resource-crd-architecture.md
│       ├── adr-005-slapddatabase-cleanup-policy.md
│       ├── adr-006-schema-lifecycle.md
│       ├── adr-007-multus-replication-network.md
│       ├── adr-008-csn-monitoring-credentials.md
│       ├── adr-009-slapduser-lifecycle.md
│       ├── adr-010-replication-modes.md
│       ├── adr-011-hot-migration-topology.md
│       ├── adr-012-seed-and-lifecycle.md
│       ├── adr-013-defer-hot-database-management.md
│       ├── adr-014-s3-backup-restore.md
│       ├── adr-015-cluster-dns-domain.md
│       ├── adr-016-pod-routed-cross-cluster-replication.md
│       ├── adr-017-bare-integer-serverid.md
│       ├── adr-018-pvc-deletion-leases.md
│       ├── adr-019-per-database-accesslog.md
│       ├── adr-020-accesslog-access-control.md
│       ├── adr-021-openldap-2.7-dual-images.md
│       ├── adr-022-syncprov-sessionlog.md
│       ├── adr-023-rolling-replacement.md
│       ├── adr-024-tunable-placement.md
│       ├── adr-025-single-creator-seed-glue-suffix.md
│       ├── adr-026-shared-cn-config-state.md
│       ├── adr-027-node-local-replication-identity.md
│       ├── adr-028-mesh-scoped-vs-site-scoped.md
│       └── adr-029-tls-on-by-default-and-legible-certs.md
├── charts/
│   ├── operator/                   # Helm chart for deploying the operator itself
│   │   ├── crds/                   # CRD YAML (synced from operator/config/crd/bases/ via make operator-manifests)
│   │   └── templates/              # deployment, RBAC, serviceaccount, metrics, networkpolicy
│   ├── slapd/                      # Helm chart deploying a SlapdCluster CR (operator required)
│   ├── slapd-mesh/                 # Helm chart deploying a whole multi-site mesh: SlapdMesh + SlapdCluster
│   │                               # + databases + schemas, applied IDENTICALLY at every site (ADR-028)
│   └── slapd-toolkit/              # Persistent debug pod (ldap-utils, python3, ldap3) wired to operator-managed Secrets
├── images/
│   ├── openldap-deb/               # Vendored Debian packaging fork → OpenLDAP 2.7.1 .debs (ADR-021)
│   ├── slapd/Containerfile         # slapd runtime image (OpenLDAP 2.7.1; Containerfile.ol26 = legacy 2.6)
│   ├── slapd-init/Containerfile    # Bootstrap init container image (2.7.1; Containerfile.ol26 = legacy 2.6)
│   ├── slapd-toolkit/Containerfile # Toolkit image (ldap-utils, python3, pyyaml, ldap3)
│   └── operator/Containerfile      # Operator image (multi-stage, distroless/static)
├── scripts/
│   └── create-remote-kubeconfig.sh # Cross-site RBAC + kubeconfig Secret provisioning (ADR-007)
├── operator/                       # kubebuilder v4 Go operator (own Go module)
│   ├── api/v1alpha1/
│   │   ├── slapdcluster_types.go   # SlapdCluster CRD (+ Restoring phase, status.restore — ADR-014)
│   │   ├── slapddatabase_types.go  # SlapdDatabase CRD (+ bootstrapFrom, restoreApplied)
│   │   ├── slapdschema_types.go    # SlapdSchema CRD
│   │   ├── slapdmesh_types.go      # SlapdMesh CRD — the site inventory a SlapdCluster resolves meshRef against (ADR-028)
│   │   ├── slapdbackup_types.go    # SlapdBackup + SlapdScheduledBackup CRDs + shared S3StorageSpec (ADR-014)
│   │   ├── slapdscheduledbackup_types.go
│   │   └── zz_generated.deepcopy.go
│   ├── internal/controller/
│   │   ├── slapdcluster_controller.go
│   │   ├── slapdcluster_restore.go # bootstrapFrom restore state machine (ADR-014 Phase 5)
│   │   ├── slapddatabase_controller.go
│   │   ├── slapdschema_controller.go
│   │   ├── slapdbackup_controller.go        # on-demand backup → co-located Job
│   │   ├── slapdscheduledbackup_controller.go # cron + retention
│   │   ├── backup_job.go           # backup Job builder (slapcat→gzip→S3)
│   │   └── restore_job.go          # restore Job builder (download→wipe→slapadd)
│   ├── config/
│   │   ├── crd/bases/              # Generated CRD YAML
│   │   ├── rbac/role.yaml          # Generated RBAC ClusterRole
│   │   └── samples/
│   │       └── ldap_v1alpha1_slapdcluster.yaml
│   ├── cmd/main.go
│   ├── go.mod                      # module: github.com/chuck-chuck-chuck-net/slaptain/operator
│   └── Makefile                    # kubebuilder-generated (generate, manifests, run, …)
└── tests/
    ├── gencert.sh                  # TLS cert generation helper
    ├── values.slapd-persistent.yaml # SlapdCluster values for the PVC-backed fixture (the only supported config per ADR-013)
    ├── e2e.sh                      # Unified e2e orchestration (no ctx / N=1 → single-site; N≥2 → multi-site)
    ├── e2e-migration.sh            # Migration-scenario e2e (independent: slaptain + fake-prod topology)
    ├── resources/
    │   ├── example/                # Open-source test fixtures (SlapdDatabase, SlapdSchema, Secrets)
    │   ├── versitygw.yaml          # Lean S3 server (Apache-2.0) for backup e2e — NOT minio
    │   └── lab/                    # Internal lab configuration (SOPS-encrypted secrets)
    ├── README.md                   # Test suite documentation (quick-start cycle at top)
    └── e2e/                        # Ginkgo e2e tests (go-ldap, client-go)
        ├── suite_test.go           # BeforeSuite: NodePort LDAP connect, rootDSE baseDN discovery, admin connect
        ├── helpers_test.go         # k8s/LDAP helpers (ldapSearch, ldapAdd, dialPodLDAP, …)
        ├── slapd_test.go           # StatefulSet, Service, PVC, passwords Secret checks
        ├── bootstrap_test.go       # SlapdDatabase Running checks
        ├── ldap_test.go            # Directory content: base structure (incl. per-pod base-DN visibility — glue suffix, ADR-025), user/group CRUD, ACL basics
        ├── readpw_test.go          # cn=config access; readpw user bind + ACL enforcement
        ├── readonly_test.go        # Read-only replica tests: data sync, write rejection
        ├── resilience_test.go      # Pod-restart resilience (warm restart labelled persistent-only; gated E2E_RESILIENCE=1)
        ├── dataloss_recovery_test.go # Pod loses its PVCs; replication restores DIT (ADR-012 case 2) + ITS#9580 no-storm assertion (ADR-021; red only on -ol26 in a multi-site mesh with a dormant SID — single-site fresh measured 0)
        ├── migration_test.go       # Migration scenario (gated at registration time: E2E_MIGRATION=1)
        ├── external_replication_test.go  # Cross-cluster replication (gated: E2E_EXTERNAL_REPL=1)
        ├── backup_test.go          # SlapdBackup → S3 round-trip (gated: E2E_BACKUP=1, deploys versitygw)
        ├── restore_test.go         # bootstrapFrom restore into a fresh cluster + glue-artifact preflight rejection (ADR-025) (gated: E2E_BACKUP=1)
        ├── restore_replay_test.go  # in-place restore under replication: marker written to the backup's SOURCE pod, asserted present in the artifact + source-honesty status; stale accesslog delete must not replay (gated: E2E_BACKUP=1)
        ├── accesslog_test.go        # Per-database accesslog: structure, no cross-DB lost-sync, convergence, ADR-020 ACL
        ├── sessionlog_test.go       # ADR-022: olcSpSessionlog on every RW pod's data DB; none on accesslog DBs
        ├── tunables_test.go         # ADR-024 degrades/hygiene tunables: cn=config posture per pod (ungated)
        ├── cleanup_policy_test.go   # ADR-005 cleanupPolicy: Delete tears a replicated DB (+ its accesslog DB) off EVERY pod; Retain leaves it; re-adoption (ungated, Label "cleanup-policy"; rolls the cluster per ADR-013)
        ├── scale_test.go            # many-entries fixture: 1200-entry seed + churn (gated: E2E_SCALE=1; knobs E2E_SCALE_ENTRIES/E2E_SCALE_CHURN)
        └── scaleup_test.go         # standalone → HA transition: schema/modules/serverID runtime convergence (gated: E2E_SCALEUP=1)
```

---

### Image Details

- **Build tooling:** Plain Containerfile + podman (apko dropped — only beneficial in the Wolfi ecosystem).
- **slapd runtime** (`images/slapd/`): OpenLDAP **2.7.1** (`2.7.1-0+slaptain1`, built from the vendored packaging fork in `images/openldap-deb/` — ADR-021) on `gcr.io/distroless/base-debian13` — glibc, libssl, ca-certs, no shell. Legacy Debian 2.6 build stays available as `slapd:<tag>-ol26` (`Containerfile.ol26`) for hot-migration interop (ADR-011). **LMDB 1.0 format break:** 2.6-written volumes cannot be opened by 2.7 — see `docs/OPENLDAP-VERSIONS.md` for the migration runbook.
- **slapd-init** (`images/slapd-init/`): `debian:trixie-slim` + the same locally-built 2.7.1 packages (slapcat/slapadd/slaptest must match the slapd version) — ephemeral bootstrap; shell + python3. `-ol26` variant pairs with the 2.6 slapd image — always pin both or neither.
- **operator** (`images/operator/`): `gcr.io/distroless/static-debian13:nonroot`, statically-linked Go binary, UID 65532. Builder stage uses `golang:1.25`.
- **User (slapd):** `openldap` (UID/GID 1024). Debian's slapd package creates this user; we `groupmod`/`usermod` to 1024.
- **Ports:** 1024 (ldap), 1025 (ldaps) — non-privileged. Service maps 389→1024 and 636→1025.
- **Mount Points:** `/config` (slapd.d config dir, PVC), `/data` (LMDB data, PVC), `/accesslog` (delta-syncrepl change journals, PVC — one `<dbname>` subdirectory per replicated database, ADR-019), `/run/openldap` (socket, emptyDir), `/etc/openldap/tls` (TLS secret, optional).
- **Module path:** `/usr/lib/ldap` (Debian path). Modules loaded dynamically; plan to compile in statically later.
- **Schema path:** `/etc/ldap/schema/` (Debian path).
- **Init Container:** Sets up `cn=config` (admin credentials, modules, TLS). Creates per-database data directories from `DATABASE_DIRS` env var. Does NOT create data databases, schemas, or ACLs.

---

### Operator Details

**Framework:** Go + kubebuilder v4 (`go/v4` plugin), controller-runtime v0.23.1, k8s API v0.35.0.

**API:**
- Group: `ldap.chuck-chuck-chuck.net`
- Version: `v1alpha1`, Scope: Namespaced
- Kinds: `SlapdCluster` (`sc`), `SlapdDatabase` (`sd`), `SlapdSchema` (`ss`),
  `SlapdBackup` (`sb`), `SlapdScheduledBackup` (`ssb`), `SlapdRestore` (`sr`)
- Backup/restore (ADR-014, `docs/BACKUP.md`): `SlapdBackup` runs an on-demand
  `slapcat`→gzip→S3 backup via a co-located Job; `SlapdScheduledBackup` emits
  them on a cron schedule with `retention{maxCount,maxAge}`; `SlapdDatabase.spec.bootstrapFrom`
  restores a backup into a fresh DB (cluster enters `status.phase=Restoring`,
  scales to 0, runs offline `slapadd`, scales back up). S3 creds via a Secret
  with keys `access-key-id`/`secret-access-key`. A backup always takes a backup
  and records its circumstances unconditionally (`status.sourcePod`,
  `status.sourceContextCSN`, condition `SourceConverged` consumed from the
  cluster's `ReplicationConverged`) — 2026-09-12 amendment; a CSN-dominance
  gate was rejected, the opt-in `requireConverged` is designed and deferred.
- In-place restore (ADR-014 amendment): `SlapdRestore` is an imperative,
  immutable-once-created request to restore a backup into an **existing**
  (possibly populated) `SlapdDatabase` — a rollback. The **SlapdCluster
  controller** watches it (no separate reconciler) and drives the *same*
  preflight → scale-to-0 → slapadd-all-pods → scale-up machine as `bootstrapFrom`,
  branching only on source resolution and completion bookkeeping
  (`status.restore.requestRef` names the driving `SlapdRestore`). Spec:
  `databaseRef` + `source.{backupRef|s3}` + `skipReplicationPasswordCheck`;
  status `phase` (Pending/Preflight/Restoring/Completed/Failed). Concurrent
  requests serialise (one restore per cluster at a time); does NOT set the DB's
  `restoreApplied` (that is the bootstrapFrom one-shot guard).

**CRD spec fields** (SlapdCluster manages infrastructure; database-level config lives on SlapdDatabase/SlapdSchema CRs — see ADR-004):

| Field | Type | Notes |
|---|---|---|
| `spec.images.{slapd,init}.{repository,tag,pullPolicy}` | `SlapdImages` | Image config for both containers. **Optional** — when omitted, the operator defaults each image to its own registry/path at its own tag (repository derived from `OPERATOR_IMAGE` by swapping the trailing path segment for `slapd`/`slapd-init`; tag from `OPERATOR_IMAGE_TAG`; canonical upstream fallback when unset). CloudNativePG-style operator-side defaulting, no CRD-baked default. Set fields to override |
| `spec.ldap.cnConfigCredentials.secretName` | string | Optional: reference an existing Secret with `root-password` key for cn=config admin; suppresses auto-generation of `<name>-config-password` |
| `spec.ldap.tls.{enabled,secretName}` | `SlapdTLSConfig` | TLS Secret must contain `tls.crt` and `tls.key`; `ca.crt` optional (public-CA certs skip it and use OpenSSL system trust) |
| `spec.replicas` | int32 | Default 1; replication is only active when `replicas > 1` AND `replication.enabled=true` |
| `spec.readReplicas` | int32 | Default 0; number of read-only consumer replicas. Requires `replication.enabled=true`. Creates a second StatefulSet `<name>-readonly` |
| `spec.logLevel` | *int32 | slapd `-d` flag; default **16640** (stats+sync, ADR-024); explicit `0` = silence |
| `spec.persistence.{config,data,accesslog}` | `SlapdPersistenceConfig` | PVC sizes / storage class / access mode (per ADR-013, persistence is mandatory; the `enabled` field was removed) |
| `spec.service.{type,ldapPort,ldapsPort}` | `SlapdServiceConfig` | ClusterIP service config, defaults 389/636 |
| `spec.resources` | `corev1.ResourceRequirements` | Container resource requests/limits |
| `spec.securityContext` | `*corev1.PodSecurityContext` | Defaults to runAsUser/runAsGroup/fsGroup=1024 |
| `spec.replication.{enabled,externalPeers}` | `SlapdReplicationConfig` | N-way multi-master delta-syncrepl; active when `enabled=true` and `replicas > 1` |
| `spec.tuning.{toolThreads,noSync}` | — | Cluster-level tuning: slapadd/slapindex tool threads; durability default inherited by DBs (ADR-024) |
| `spec.ldap.passwordHash` | string | Default `{SSHA}`, converged (ADR-024) |
| `spec.ldap.tls.{protocolMin,cipherSuite}` | — | TLS floor default `3.3`; cipher policy opt-in, deliberately undefaulted (ADR-024) |
| `spec.backend.idlExponent` | `*int32` | back-mdb IDL exponent — **bootstrap-time** (ADR-024 R2): applied by the init container before any DB exists; no operator default (a later default change would split a cluster); change path is the documented recreate |
| `spec.replication.network.mode` | string | Cross-cluster transport: `pod-routed` (default; primary pod IPs on a natively cross-site-routed pod network, no NAD/operator-NIC, ADR-016) or `multus` (net1 IPs, ADR-007). When unset, defaults to pod-routed unless `multusNetwork` is set (then multus) |
| `spec.replication.network.multusNetwork` | string | NAD reference for dedicated replication network (e.g. `infra/replication-net`). Required for `mode: multus`; omit for `pod-routed`. See ADR-007 |
| `spec.replication.network.useForInCluster` | bool | Use Multus IPs for in-cluster syncrepl too (default false; multus mode only) |
| `spec.replication.keepalive` | string | TCP keepalive for syncrepl connections (e.g. `idle:probes:interval`) |
| `spec.replication.retry` | string | Retry interval for syncrepl connections (e.g. `60 +`) |
| `externalPeers[].discovery` | `*ExternalPeerDiscovery` | Dynamic peer discovery via remote k8s API (ADR-007 amendment). Mutually exclusive with `uri` and `podAddresses` |
| `externalPeers[].discovery.kubeconfigSecret.{name,key}` | `KubeconfigSecretRef` | Secret containing kubeconfig for remote cluster (key default: `kubeconfig`) |
| `externalPeers[].discovery.namespace` | string | Remote SlapdCluster namespace (default: local namespace) |
| `externalPeers[].discovery.clusterName` | string | Remote SlapdCluster name (default: local name) |
| `externalPeers[].replicasPerPeer` | `*int32` | Cross-site fan-out (default 1). Local pod `i`, connection `k` → remote pod `(i+k) % N`. Capped at address count. Ignored in `uri` mode |

Database-level config (ACLs, schemas, indices, replication per-DB) is declared on `SlapdDatabase` and `SlapdSchema` CRs. Per-DB tunables (`maxSize` — recreate-only, slapd segfaults on live modify; `sizeLimit`/`timeLimit`/`limits` default unlimited; checkpoint, `noSync`, `envFlags`, `accesslogPurge` default `7+00:00 1+00:00` with `"none"` opt-out, `syncprovCheckpoint`) follow ADR-024's placement classes — see `docs/TUNING.md`.

**Status fields:** `phase` (Bootstrapping/Running/Degraded/Error/**Restoring**), `readyReplicas`, `replicas`, `readOnlyReadyReplicas`, `readOnlyReplicas`, `observedGeneration`, `replicationNetworkIPs` (discovered Multus IPs per pod), `externalPeerStatuses` (per-peer: `replicationState` Synced/Lagging/**PartiallyVerified**/Unreachable, `lagSeconds`, `lastChecked`, `discoveredAddresses`), `restore` (in-progress bootstrapFrom restore: sub-`phase`, `originalReplicas`, `databases` — ADR-014), `conditions` (including `ReplicationConverged` for local CSN convergence).

**CSN convergence is judged per database** (ADR-008 amendment 2026-09-14). A `contextCSN`
vector belongs to one database on one pod and is only comparable to the same database's
vector elsewhere; `ReplicationConverged` is the AND over databases (`CSNsMatch` /
`CSNsDiverged` naming the database / `CSNQueriesIncomplete` when a pod×database pair could
not be read — unreadable evidence never counts toward True). `PartiallyVerified` is the
peer-side counterpart: the peer answered and everything read is current, but at least one
database yielded no readable contextCSN (`lastError` names it).

**Reconcile order (SlapdCluster controller):**
1. Fetch `SlapdCluster` — NotFound → return nil (deleted)
2. `reconcileSecret` — create `<name>-config-password` (plaintext `root-password`, create-only); or read from `spec.ldap.cnConfigCredentials.secretName`
3. `reconcileHeadlessService` — `<name>-headless`, `clusterIP: None` (SSA patch)
4. `reconcileClusterIPService` — `<name>` (bare name), ClusterIP (SSA patch)
5. `reconcileStatefulSet` — `serviceName: <name>-headless`; queries SlapdDatabase CRs to compute `DATABASE_DIRS` env var for init container; provisions `volumeClaimTemplates` for config/data/accesslog (persistence is mandatory per ADR-013) (SSA patch)
5a. `reconcileReadOnlyHeadlessService` — `<name>-readonly-headless`, `clusterIP: None` (skipped when `readReplicas=0`)
5b. `reconcileReadOnlyService` — `<name>-readonly`, ClusterIP (skipped when `readReplicas=0`)
5c. `reconcileReadOnlyStatefulSet` — second StatefulSet for RO consumers: no accesslog, `LDAP_READONLY_REPLICA=true` (skipped when `readReplicas=0`)
6. Observe StatefulSet → update `status.phase`, `readyReplicas`, `externalPeerStatuses`, conditions (SSA patch on status subresource)
7. Not Running → `RequeueAfter: 10s`

**Note:** Bootstrap, ACL management, schema management, and replication configuration have moved to the `SlapdDatabase` and `SlapdSchema` controllers (see ADR-004).

**Service naming (Bitnami convention):**
- Headless: `<name>-headless` — used by StatefulSet for pod DNS (`<name>-0.<name>-headless.ns.svc`)
- ClusterIP: `<name>` — client-facing, maps standard ports 389→1024 and 636→1025
- This ensures `SLAPD_HOST=slapd` resolves the same way for every deployment of the chart
- The `slapd` chart is itself named `slapd`, so `helm install slapd ./charts/slapd` → fullname `slapd`

**Owned resources:** StatefulSet, Service (×2), Secret, PersistentVolumeClaim — all get `SetControllerReference`.

**Known gotcha:** `kubebuilder init` requires `--skip-go-version-check` on Go 1.26 (version string not recognized). `kubebuilder create api` does not accept this flag and works without it.

---

### Test Resources (`tests/resources/`)

Test fixtures are plain Kubernetes manifests (SlapdDatabase, SlapdSchema, Secret) applied
via `kubectl apply`.

- `tests/resources/example/` — open-source test fixtures suitable for CI and getting started.
- `tests/resources/lab/` — internal lab configuration (SOPS-encrypted secrets, additional schemas).

Deploy with `make testing-apply`, remove with `make testing-delete`.
Or use `./tests/e2e.sh all <context> [more-contexts...]` for an all-in-one cycle —
one context for single-site, two or more for multi-site.

### slapd-toolkit chart (`charts/slapd-toolkit/`)

A debug-only chart deploying a long-running pod from the `slapd-toolkit` image. Defaults
target `clusterName: slapd` + `dbName: default`, wiring `LDAP_ADMIN_PW` from
`<dbName>-credentials` and `LDAP_ROOT_PW` from `<clusterName>-config-password`. Use it via
`make toolkit-install` or as the image for `kubectl debug --target=slapd`.

### e2e Test Suite (`tests/e2e/`)

**Typical cycle:**
```bash
make cluster-helm-install testing-apply
make e2e-run
make testing-delete cluster-helm-uninstall
```

Or all-in-one: `./tests/e2e.sh all <context> [more-contexts...]` (single-site with N=1, multi-site with N≥2)

**The suite deploys through `charts/slapd-mesh` (ADR-028).** There is one path, not
two: `e2e.sh` builds a single values file, applies it unchanged at every site, and
the operator derives each site's `serverIDBase`, `externalPeers`, network mode and
trust wiring from the `SlapdMesh` plus its own `SITE_NAME`. The Nth context is the
logical site `site-N` at `serverIDIndex` N−1, reproducing the historical decades
(0/100/200) exactly. The hand-wired path — and with it `E2E_MESH`, the peer loop and
the `serverIDBase` arithmetic — was deleted in Phase 7. A multi-site run now requires
a discovery transport (`pod-routed` or `multus`) and refuses to start without one;
NodePort `uri` and static Multus `podAddresses` peers are consequently untested
(`docs/BACKLOG.md`). User-facing guide: `docs/MULTI-SITE.md`.

**Node access (NodePort reachability):** the runner reaches slapd via NodePorts on
a per-context *node access IP*, defaulting to each node's k8s `InternalIP`. On
dual-homed clusters that default is wrong when the `InternalIP` is on a network the
runner can't reach north-south (e.g. a routed replication network chosen as the
primary node network) — and the reachable NIC isn't k8s-registered, so it can't be
auto-discovered. Override it: `E2E_NODE_ACCESS_IP=<ip>`
(single-site) or `E2E_NODE_ACCESS_IPS="ctx=ip ..."` (multi-site) — or describe
the lab once in a gitignored `<repo-root>/lab.yaml` (schema: `lab.yaml.sample`;
env vars win; `./tests/e2e.sh config` dumps the resolved values; sites double
as the default context list). This drives
`LDAP_ADDR`/`E2E_REMOTE_LDAP_ADDR`, the suite's `E2E_NODE_IP`, and the TLS cert
SAN; cross-site peer URIs keep the `InternalIP` (they must ride the replication
network). See `tests/README.md`.

**Failed restore specs keep their evidence.** `restore*_test.go` skip teardown
when a spec fails (`E2E_KEEP_ON_FAILURE=0` to opt out) and print a post-mortem —
CR statuses, pod/container states, init + slapd logs, events — into the spec
output before anything is deleted. Green runs delete their
`volumeClaimTemplates` PVCs too. Run the bootstrapFrom spec alone with
`E2E_LABEL_FILTER=restore-bootstrap`.

**Backup/restore e2e** (gated `E2E_BACKUP=1`): `E2E_BACKUP=1 ./tests/e2e.sh test <ctx>`
deploys `tests/resources/versitygw.yaml` (lean Apache-2.0 S3 server — NOT minio)
and runs `backup_test.go` + `restore_test.go`. The restore spec stands up a
second single-replica `slapd-restore` cluster and exercises the full scale-to-0
restore machine. See `docs/BACKUP.md`. (For iterating on a lab cluster: pin
images with `GIT_TAG=<pushed-tag>` and use `./tests/e2e.sh setup <ctx>` once,
then re-run `test`.)

**Suite setup** (`suite_test.go` `BeforeSuite`):
1. Build k8s client
2. Wait for `slapd` StatefulSet ready
3. Wait for SlapdDatabase to reach `Running` phase
4. Read `adminPW` from `<dbname>-credentials` Secret (`root-password` key)
5. Read `rootPW` from `slapd-config-password` Secret (`root-password` key)
6. Read `readpwPWs` from `slapd-test-passwords` Secret — `map[string]string` built from all `readpw-*` keys
7. Dial `LDAP_ADDR` (NodePort, set by the e2e scripts) and query rootDSE (anonymous, `namingContexts`) → set `baseDN` (auto-discovered, no env var)
8. Connect `ldapConn` as `cn=admin,<baseDN>` (data rootDN, bypasses ACLs)

**readpwOU** is configurable via `READPW_OU` env var (default: `ServiceAccounts`).

**Readpw ACL tests** (`readpw_test.go`) skip gracefully when `readpwPWs` is empty, so the suite
runs without readpw configuration but skips those test cases.

### Makefile Targets (root)

| Target | Effect |
|---|---|
| `make all` | Build all images (2.7 slapd pair, `-ol26` 2.6 pair, toolkit, operator) |
| `make build-init` | Build slapd-init image |
| `make build-slapd` | Build slapd image |
| `make build-operator` | Build operator image (build context = repo root) |
| `make push` | Build + push all six images (both slapd pairs, toolkit, operator) |
| `make build-openldap-deb` | Build the local-only OpenLDAP 2.7.1 .deb carrier image (feeds both 2.7 image builds; never pushed) |
| `make build-ol26` / `make import-ol26` | Build / CRI-import the legacy 2.6 slapd+init pair (`:<tag>-ol26`, ADR-021) |
| `make operator-generate` | Run `make generate` in `operator/` (regenerates deepcopy) |
| `make operator-manifests` | Run `make manifests` in `operator/`, then sync CRD to `charts/operator/crds/` |
| `make operator-sync-crd` | Copy CRD from `operator/config/crd/bases/` to `charts/operator/crds/` |
| `make operator-crd-apply` | `kubectl apply --server-side` the CRDs from `operator/config/crd/bases/` (Helm only installs `crds/` on first `install`, never on `upgrade`) |
| `make operator-helm-install` | `helm upgrade --install slaptain ./charts/operator` (depends on `operator-crd-apply`, so a new/changed CRD lands on upgrade too). t3e loop: `make operator-helm-install CONTEXT=t3e GIT_TAG=<tag>` |
| `make operator-helm-uninstall` | Uninstall the operator Helm release |
| `make gencert` | Generate self-signed TLS cert via `tests/gencert.sh` |
| `make helm-deploy` | Full pipeline: `deliver` + `gencert` + `cluster-helm-install` |
| `make cluster-helm-install` | `helm upgrade --install slapd ./charts/slapd` |
| `make cluster-helm-uninstall` | Uninstall the slapd Helm release |
| `make testing-apply` | `kubectl apply` test resources (SlapdDatabase, SlapdSchema, Secrets) |
| `make testing-delete` | `kubectl delete` test resources |
| `make toolkit-install` | `helm upgrade --install toolkit ./charts/slapd-toolkit` (debug pod), image pinned to `GIT_TAG` |
| `make charts-package` | Package every chart in `PUBLISH_CHARTS` (operator, slapd-mesh, slapd, slapd-toolkit) into `.charts/`, with `--version`/`--app-version` from the git tag |
| `make charts-push` | Push all packaged charts to `$(CHART_REGISTRY)` (`helm registry login` first) |
| `make operator-chart-package` / `make operator-chart-push` | The operator chart alone (same mechanism) |
| `make toolkit-uninstall` | Uninstall the toolkit Helm release |
| `make e2e-run` | Run Ginkgo e2e tests in `tests/e2e/` (requires NodePort cluster pre-deployed) |
| `make e2e-multisite CONTEXTS="c1 c2"` | All-in-one multi-site e2e cycle via the unified `tests/e2e.sh` |
| `make e2e-external-replication` | Run cross-cluster external replication tests (E2E_EXTERNAL_REPL=1) |

### Makefile Targets (operator/)

| Target | Effect |
|---|---|
| `make generate` | Regenerate `zz_generated.deepcopy.go` |
| `make manifests` | Regenerate CRD YAML and RBAC `role.yaml` |
| `make run` | Run operator locally against current kubeconfig |
| `make install` | `kubectl apply` the CRD |
| `make build` | Compile the manager binary |

---

### Local Dev Workflow

```bash
# 1. (If types changed) Regenerate deepcopy, CRD manifests, and sync to chart
make operator-generate operator-manifests   # operator-manifests includes operator-sync-crd

# 2. Deploy operator via Helm (installs CRD + RBAC + Deployment)
make operator-helm-install
# Or for development without building/pushing an image, run locally instead:
cd operator && make run

# 3. Prereqs for a SlapdCluster: TLS secret (gencert creates the
#    slaptain-testing namespace itself and puts slapd-tls there)
make gencert

# 4. Apply sample CR into the same namespace
kubectl apply -n slaptain-testing -f operator/config/samples/ldap_v1alpha1_slapdcluster.yaml

# 5. Verify
kubectl get sc -n slaptain-testing
kubectl get statefulset,svc,secret,pvc -n slaptain-testing -l app.kubernetes.io/instance=slapd
kubectl rollout status statefulset/slapd -n slaptain-testing --timeout=120s

# 6. Phase 1 guard smoke test: apply with replicas:2, verify status.phase=Error
```

### Important Notes
- **No kustomize.** All deployment is via Helm. The `operator/config/` tree is kubebuilder scaffolding only — used to generate code/CRDs, not applied directly to clusters.
- **CRD sync:** `charts/operator/crds/` is populated from `operator/config/crd/bases/` by `make operator-manifests`. Always run `make operator-manifests` after changing types and commit both the generated CRD and the chart copy together.
- **CRDs on upgrade:** Helm installs a chart's `crds/` **only on the first `helm install`**, never on `helm upgrade`. So a new or changed CRD will be missing/stale after an operator upgrade. `make operator-helm-install` depends on `make operator-crd-apply` to handle this; if you upgrade with a raw `helm upgrade`, also run `make operator-crd-apply` (or `kubectl apply --server-side -f operator/config/crd/bases/`).

---

### References

This repository is published as a stand-alone open-source project, so every
reference must point to something publicly available (upstream projects, RFCs,
public docs and URLs). Do not add references to unpublished or private resources:
internal repos, internal design docs, ticket IDs, cluster/host/domain names, IPs,
or absolute local paths — they are meaningless to an external reader. Use neutral
placeholders or RFC-reserved examples instead (`<context>`, `<reachable-node-ip>`,
`k8s.example`). When unsure whether something is public, ask before adding it.

### Fix Discipline

Do not fix bugs quickly or lightheartedly. A fix lands only after:

1. **Full root-cause understanding** — the actual mechanism, demonstrated (logs, live
   cluster state, a reproduction), not a plausible-sounding theory. Prefer the explanation
   that also accounts for why it *used* to work. If a first theory doesn't, keep digging;
   e.g. the ADR-015 bug looked like a readiness/DNS-publishing issue until `/etc/hosts`
   showed the real cause was a hardcoded cluster domain.
2. **Regression & side-effect analysis** — check the relevant ADRs (follow them; if the fix
   conflicts with one, discuss before proceeding) and `docs/reconcile-loop-fixes.md` for
   prior art. Map the full blast radius, not just the first symptom. Confirm the default
   path stays unchanged.
3. **e2e coverage** — either an existing e2e exercises the fix, or one is added alongside it.
   Build + `go vet` + unit tests are necessary but not sufficient.

Record the outcome: an ADR when the fix establishes a pattern future code must follow, and a
`docs/reconcile-loop-fixes.md` entry for any reconcile/replication bug.

### Test Discipline

Test-first, red-first. The prior habit — authoring tests in the same pass as the
implementation — produces tests that only mirror the code's assumptions (bugs included):
they pass, but were never shown to catch anything.

**The rule:** every test must be observed to FAIL, for the right reason, at least once
before it counts as coverage. A test that never went red is a mirror, not a check. Author
the check *before* the implementation and show the failing run first; then make it pass.
This matters most under agentic coding — an agent testing its own just-written code tends to
codify its own mistakes and report green, so the red is the only thing that proves the test
has teeth.

Applied per tier:

1. **Bugs → failing repro first.** Write the reproduction as a committed test, watch it go
   red, then fix to green. (This is Fix Discipline §1's "reproduction" promoted to a test,
   with the red shown before the fix.)
2. **New pure/unit-testable logic → assertion first.** Write the assertion straight from the
   ADR/spec, see it fail, implement to green. Fast red-green lives here.
3. **e2e-only behavior (reconcile/replication) → target assertion first.** Adjust the e2e to
   the intended behavior, confirm it is red against current code, then implement (slow loop
   accepted). Design corollary: push the *decision* into a thin pure function (e.g.
   `desiredServerID`) so it is unit-TDD-able fast, leaving e2e a thin integration shell.

**Replication-path changes see a mesh before merge.** A change that alters
syncrepl stanzas, overlays, or anything else on the replication path gets one
multi-site e2e cycle before merging — or the commit message states explicitly
that multi-site is unvalidated. Single-site fixtures structurally lack the long
refresh windows and convergence write-bursts where this class of regression
lives (the timeout= freeze was invisible on t3e and cost ~300 s full-server
silences on the mesh).

**Not dogmatic:** behavior-preserving refactors under already-green tests need no new red —
the existing tests are the guard. Red-first applies to new behavior and bug fixes. If a test
will not go red for the intended reason, treat it as a broken test and say so — do not paper
over it.

### Architecture Decision Records (ADRs)

ADRs in `docs/adrs/` record significant design decisions. They are a first-class artifact —
treat them the same as code.

**Before making a design decision:** Check existing ADRs. If a relevant ADR exists, follow it.
If the current task conflicts with an ADR, discuss with the user before proceeding — do not
silently violate an ADR.

**When to create a new ADR:** Any decision that constrains future implementation choices,
rejects a plausible alternative, or would be non-obvious to a new contributor. Examples:
choosing between two architectural approaches, deciding on a data model, establishing a
pattern that other code must follow.

**When to amend an existing ADR:** When a decision's scope expands but the core principle
holds. Add an "Amendment" section at the bottom with the date and rationale. Do not rewrite
the original decision — the history of reasoning matters.

**ADR format:**
- Status (Proposed / Accepted / Superseded), Date
- Context: what problem prompted the decision
- Options considered (with reasons for rejection)
- Decision: what was chosen and why
- Consequences: what follows from the decision
- Related: links to other ADRs

**Current ADRs:**
- ADR-001: Double reconciliation runs are harmless (idempotency requirement)
- ADR-002: cn=config is node-local; the operator manages it per-pod — *amended 2026-08-26: per-pod convergence is event-driven and its only periodic trigger is incidental (the 60s `lastChecked` status churn); filtering the SlapdCluster watch stops convergence, and a non-replicated cluster has no periodic resync at all*
- ADR-003: Operator owns all syncrepl configuration (RID scheme, single source of truth)
- ADR-004: Multi-resource CRD architecture (SlapdCluster / SlapdSchema / SlapdDatabase)
- ADR-005: SlapdDatabase cleanup policy (Retain default, Delete opt-in) — *amended 2026-09-14: the decision was never implemented for the databases it exists for — a replicated data DB is not a leaf and slapd does not cascade, so `Delete` always failed and the finalizer was released anyway. Teardown is now subtree-deepest-first, reaps the database's own `cn=accesslog-<db>` (data DB first, so no dangling `olcAccessLogDB` — ADR-026), covers RO pods, and a failed teardown keeps the finalizer and retries as the ADR always said it would*
- ADR-006: Schema lifecycle (additive-only, desired-minimum model)
- ADR-007: Multus-based dedicated replication network for cross-site traffic (amended: dynamic peer discovery via remote kubeconfig)
- ADR-008: CSN monitoring uses replication bind credentials (uniform-password assumption) — *amended 2026-08-26: what CSN comparison can and cannot observe — peer status is consumer-side and one-directional, and lag is only observable while writes flow, so an idle database reads `Synced` across a broken link; amended again 2026-09-11: the "heartbeats only help during idle periods, when nothing is at stake" premise was refuted by ITS#9580 dormancy (idleness lets a SID's minCSN go stale; the next reconnect pays with a stale-cookie full-refresh storm) — heartbeats stay deferred regardless, superseded by ADR-021 (OpenLDAP 2.7) and ADR-022 (syncprov sessionlog); amended 2026-09-15: the monitoring bind now tries `cn=repl-<db>,cn=slaptain-auth` first and the legacy DN second (the fallback exists for cross-site peers on a pre-ADR-027 operator, which would otherwise read Unreachable for the whole migration window), and "create-only, never rotates" is retired — the Secret is the source of truth and rotation is supported, except during the ADR-027 migration window*
- ADR-009: SlapdUser lifecycle (service users only, single-pod write, retain default) — *Proposed*
- ADR-010: SlapdCluster replication modes (peer / consumer-only, in-place promotion) — *Accepted (impl + e2e green 2026-05-13)*
- ADR-011: Hot migration topology contract (RID/ServerID coexistence, plain-syncrepl interop, stage transitions) — *Accepted (impl + e2e green 2026-05-13)*
- ADR-012: Seed is one-shot; cluster wipe is a Kubernetes resource lifecycle operation (replaces removed `forceRebootstrap` + reverted `verifySeedExists`)
- ADR-013: Defer hot SlapdDatabase add/remove; require persistent storage (rolling restart on DB add/remove accepted as UX wart on persistent storage)
- ADR-014: S3 backup/restore (slapcat→gzip→S3 via co-located Job; bootstrapFrom restore into a fresh DB) — *Accepted (impl + e2e green 2026-06-09; amended 2026-08-24: `SlapdRestore` is a rollback only without external peers — on a mesh member peers replay newer changes, so it is a local re-seed; true rollback is a mesh-wide human runbook)*
- ADR-015: Cluster DNS domain is resolved (CLUSTER_DOMAIN env → resolv.conf → cluster.local), never hardcoded — *Accepted 2026-07-05*
- ADR-016: Direct native pod-IP routing as a cross-cluster replication transport (alongside Multus/NodePort; `network.mode: pod-routed`) — *Accepted (impl + e2e green across three routed-pod-CIDR sites 2026-07-06)* — *amended 2026-08-26: replacing a pod invalidates every peer's syncrepl addresses, including via the scale-to-0 restore machine; measured recovery 130-268s, because address discovery rides on the 60s CSN-monitoring tick*
- ADR-017: `olcServerID` is a bare integer (`serverIDBase + ordinal + 1`), not the URL-list self-match form — sheds the FQDN/cluster-domain coupling that made serverID the ADR-015 boot crash surface — *Accepted 2026-07-17*
- ADR-018: Co-located PVC access — RWO is per-node, but any pod object naming a PVC (finished pods included) is a deletion lease that blocks pod re-roll; the operator reaps every Job it creates — *Accepted 2026-08-23*
- ADR-019: One accesslog DB per replicated data DB (`cn=accesslog-<dbname>` at `/accesslog/<dbname>`) — a shared log makes every write to one DB kick the other DB's consumers into full refresh; verified against slapd sources and upstream guidance — *Accepted (impl + e2e green 2026-08-25; amended twice the same day from the first live run — R8 step order + reference-counted delete, then the "never reuse an olcDatabase={N} DN across a delete" rule and per-pod stanza deferral; Consequences corrected: a missing `logbase` **halts** replication, it does not degrade to full refresh; amended 2026-09-12: adopts upstream's full index set — adds `reqDN`, `entryCSN`, `objectClass`, drops the no-op `default eq` — converged onto existing log DBs; amended 2026-09-14: **R8 withdrawn** — the operator no longer migrates a legacy cluster-shared log. Its reference-counted delete authorised itself from other databases' overlays (state the operator does not own) and `DropOverlay` was gated on the database it deletes, so a stranded `olcAccessLogDB` was unrepairable and fatal at the pod's next startup — slapd validates `logdb` online but resolves it offline in `accesslog_db_open`. Replaced by a manual runbook + an `slctl inspect` issue; the rule is ADR-026)*
- ADR-021: OpenLDAP 2.7.1 by default, built from a vendored Debian packaging fork (`images/openldap-deb/`); the 2.6 pair stays as `:<tag>-ol26` for hot-migration interop (ADR-011). ITS#9580 is fixed in no 2.6.x release and no distro ships 2.7; LMDB 1.0 makes 2.6→2.7 a dump/reload (`docs/OPENLDAP-VERSIONS.md` runbook) — *Accepted (images + single-site e2e green on 2.7.1 2026-09-11; the ITS#9580 storm red and mixed-mesh validation belong to the deferred multi-site session — a fresh single-site cluster measured 0 storm lines even on 2.6)*
- ADR-022: The syncprov sessionlog belongs on the data DB only — never an accesslog DB, where a successful replay would displace the minCSN guard and under-replicate silently. On by default at 5000 ops via `spec.replication.syncprovSessionlog` (tristate: unset→5000, 0→off, >0 verbatim) — *Accepted (unit red-first + e2e red/green on live clusters 2026-09-11)*
- ADR-023: Rolling volume replacement — an imperative, one-shot `SlapdRollingReplace` rebuilds pods from the mesh one ordinal at a time (ADR-012 case 2 promoted to an operation), gated on surviving redundancy and CSN convergence; headline use case: the 2.6 → 2.7 LMDB format break (ADR-021) without the ADR-014 outage — *Proposed 2026-09-12, implementation after v0.1.0*
- ADR-024: Where an OpenLDAP tunable lives — three placement classes (converged-per-pod as the default, bootstrap-time, create-only-by-nature), a spec field that cannot be honoured is rejected or converged but never ignored, and unset means slaptain's default rather than slapd's — *Accepted 2026-09-12; amended twice from live evidence: olcDbMaxSize and olcDbEnvFlags segfault slapd on a live modify, olcIdleTimeout/olcWriteTimeout and the monitor ACL MOD hang it — R1 membership is proven on a running pod. User-facing guide: docs/TUNING.md*
- ADR-025: Seed is single-creator MESH-wide (founder-only: exactly one site carries `spec.seed`; peers sync). A multi-site seed race demotes one pod's suffix entry to a permanent hidden GLUE — invisible to ordinary searches (ManageDSAIT reveals it), frozen at the winner's entryCSN so all CSN health reads clean, silently breaking that pod and making every backup from it unrestorable. Fix set: e2e founder-only seeding; operator withhold belt (foreign-sid suffix creator → seed withheld — the OPPOSITE direction of the reverted verifySeedExists); restore preflight rejects a glue/RDN-less suffix entry (destroy-last); `SourceSuffixHealthy` on SlapdBackup; `DataPresent` all-pods; `slctl inspect` suffix-visibility + suffix-uuid-agreement; NO auto-heal (manual runbook in the ADR) — *Accepted 2026-09-13; amended 2026-09-14: following Decision 1 switched OFF Decision 5 — a never-seeded database (every non-founder site, migration, consumer-only, bootstrapFrom) never latched `SeedApplied`, so the per-pod suffix probe never ran; measured 4 of 6 databases on a healthy mesh. The probe is now ungated and the seed latch informs only what an all-empty reading means (`Unknown/NoDataYet` vs `False/DataMissing`, via the new one-way `status.dataObserved`); a ManageDsaIT-confirmed glue reports `False/GlueSuffix` on its own. One shared probe in `internal/suffixprobe` for operator, backup and slctl; amended again 2026-09-14: the all-pods rule now includes the **read-only fleet** — an RO-only divergence reports `False/DataMissingOnReadOnlyPods` (its own reason, because an RO initial sync makes it legitimately transient), phase-neutral, byte-identical at `readReplicas: 0`*
- ADR-026: What a `SlapdDatabase` reconcile may do to `cn=config` state it does not exclusively own — R1 `olcDatabase={N}` is a positional namespace, so re-resolve after any delete (implemented, correct); R2 never destroy shared state on evidence the operator does not own (another database's overlays, a hand edit — ADR-002 sanctions both; report and stall instead, because the bad state here is silent until a restart); R3 trigger a repair on the condition it repairs, never on a sibling artifact the same pass may delete. Origin: the withdrawn ADR-019 R8 migration stranded an `olcAccessLogDB` and made a pod permanently unbootable. R2/R3 are forward-looking guards — after R8's removal no live path violates them — *Accepted 2026-09-14*
- ADR-027: The replication identity is node-local, not an entry in the replicated tree — `cn=repl-<db>,cn=slaptain-auth` in a per-pod, never-replicated auth database on the data PVC, converged from the Secret (so rotation finally works). Kills the two-store class at its cause: the duplication is inherent to simple bind, but putting the verifier copy in the *replicated customer tree* is what made it shared multi-writer state (ADR-026 R2) and produced both the 19-day dead db2 link and a password-stripped copy. Fixed infrastructure: outside `DATABASE_DIRS`, so no new ADR-013 roll. SASL EXTERNAL/mTLS considered and deferred with its real marginal value recorded — *Accepted 2026-09-15 for what a single site can show; amended twice: 2026-09-14 (M1, the database + entries, additive) and 2026-09-15 (M2, the cutover — stanzas, ACLs and olcLimits and the operator's CSN bind all name the node-local DN, with the legacy grants RETAINED; identity convergence is now an ordering GATE that withholds syncrepl until every RW pod carries the entry; rotation measured at 52 s across four pods). Two live findings: the migration window is one-directional (a stanza carries one binddn, so a NEW consumer against an OLD provider fails — pin `externalPeers[].bindDN` meanwhile), and rotating the Secret mid-window strands the create-only legacy entry. Mesh-validated 2026-09-15 (86/2; the two failures were the gate firing on TLS-less fixture clusters whose auth database a pre-existing short-circuit never created — `runConvergenceSteps`, see docs/reconcile-loop-fixes.md). Mixed-VERSION mesh behaviour remains reasoned, not measured*
- ADR-028: The mesh is a layer of its own — `SlapdMesh` (sites, network, trust) ← `meshRef` ← `SlapdCluster` (+ a `sites` subset selector) ← `clusterRef` ← `SlapdDatabase`/`SlapdSchema`. A mesh-scoped resource carries **no per-site fields**, so it is byte-identical everywhere and parity is existence + a hash; the single per-site fact is `SITE_NAME` on the operator's own chart, never in a CR. Four of nine cross-site invariants stop existing (the operator derives `serverIDBase` from a **declared, not positional** `serverIDIndex`, plus `externalPeers`, network mode and trust wiring); the rest survive as one chart applied identically. `spec.seed.site` replaces the e2e's seed-stripping, making ADR-025's single-creator rule a property of the spec rather than of a deployment procedure. Packaging unit is a chart, not a fused CR. `meshRef` + an explicitly set derived field is refused, never silently preferred. Operator fan-out to peer clusters rejected (ADR-026 R2) — *Accepted 2026-09-18: all 7 MESH-PLAN phases landed, three-site lab at 88/93 with zero failures, decades reproduced exactly. Deleting the hand-wired e2e path cost the only coverage of NodePort `uri` and static Multus `podAddresses` peers (docs/BACKLOG.md); the mesh `multus` branch is written but untested*
- ADR-029: TLS is on unless the user turns it off, and a missing certificate is a legible state — `enabled` becomes `*bool` (unset → on), `secretName` defaults to `<name>-tls`, and the cluster controller gates the StatefulSet roll on the Secret existing with `tls.crt`/`tls.key`, reporting `TLSReady=False` and phase Pending instead of leaving pods in ContainerCreating with nothing on the CR. Withholds, never tears down (ADR-027 shape). Origin: 0.2.0 made the charts stop restating operator defaults, and TLS was the last value a chart held that the product should — *Proposed 2026-09-19, implementation after v0.2.0*
- ADR-020: An accesslog DB is at least as restrictive as the database it journals — *amended 2026-09-12: the replication identity also gets unlimited olcLimits on the data DB and the journal (slapd's default sizelimit of 500 capped every syncrepl search — found when a journal outgrew it live); never modify olcDbMaxSize on a live database (slapd segfaults — ADR-024 amendment)* — `to * by dn.exact="cn=repl-<db>,cn=slaptain-auth" read by dn.exact="cn=replication,<suffix>" read by * none` (two `by` clauses since the ADR-027 cutover, 2026-09-15 amendment — one rule, still ending `by * none`; the `olcLimits` exemption doubles with it, because a limits value carries exactly one selector); without it a data DB's ACLs are bypassable through its own change journal — *Accepted (impl + e2e green 2026-08-25; the bypass was captured live before the fix — an anonymous read of the shared journal returned `reqMod: userPassword:+ {SSHA}…` for a user whose `userPassword` the data DB denies)*

---

### slctl — Diagnostic CLI

`bin/slctl` (also installed to `/usr/local/bin/slctl`) is a diagnostic and management tool
for SlapdCluster resources. Source: `operator/cmd/slctl/`. Use it when debugging e2e failures
or inspecting cluster state.

| Command | Purpose |
|---|---|
| `slctl status [-n ns] [name]` | Quick overview: phase, replicas, replication, conditions |
| `slctl inspect [-n ns] [name]` | Per-pod LDAP queries + automated consistency checks (CSN convergence, topology, stanza counts). Per-database quantities are gathered and judged **per data suffix** — `contextCSN` is probed for every non-`cn=` naming context and the `csn-convergence` verdict is the worst over databases, naming the one it indicts (ADR-008 amendment 2026-09-14), alongside the ADR-025 suffix probes. `--short` for CI (checks only — every database's verdict still shows). `--json` carries `pods[].contextCSNBySuffix`. Exits non-zero on check failure |
| `slctl debug-dump [-n ns] <name>` | Collect CR YAML, pod logs, LDAP state (rootDSE, contextCSN, syncrepl, ACLs), services, PVCs, events, operator logs into a timestamped directory |
| `slctl debug [cluster] [--pod <ord>] [-- cmd...]` | Attach an ephemeral **slapd-toolkit** debug container to a slapd pod (`kubectl debug --target=slapd`, defaults to `bash`; slapd is PID 1 there, its LDAP on `localhost:1024`). **Auto-detects the namespace's PodSecurity level** via a server-side dry-run probe and adapts: **privileged** → root + `SYS_PTRACE`/`SYS_ADMIN`/`NET_ADMIN`/`NET_RAW`, slapd's *live* FS at `/proc/1/root/`, `strace -p 1` works; **baseline** → root, no added caps, slapd PVCs mounted **read-only** at `/config`/`/data`/`/accesslog` (warns, prints the label command to unlock full power); **restricted** → UID 1024 + the same read-only mounts. `--no-privileged` forces the mount mode. Toolkit image auto-derived from the target pod's *running* slapd image (not the CR spec, so it works when `spec.images` is operator-defaulted; `--image` to override). The slapd container's own hardened security context is never touched. NB: `kubectl debug` ephemeral containers persist on the pod until it is recreated |
| `slctl ldapsearch [slctl-flags] [ldapsearch-args...]` | Wraps system `ldapsearch` with auto-discovered `-H`/`-D`/`-w`. Endpoint preference: LoadBalancer → NodePort → direct pod IP → port-forward. `--as admin\|config\|replication\|<DN>` selects the bind identity (default `admin`); `--anonymous` skips the bind. `--pod <ord>` targets one pod (RW or RO) — direct pod IP when reachable, else a port-forward. `--ldaps` for TLS. `--cluster`/`--database` only needed when the namespace has more than one. **Direct pod IPs (pod-routed labs / on-node shells):** whether the client can reach pod IPs directly is a property of the network path, so it is **autodetected, not configured**: a specific (non-default) kernel FIB route covering the pod IP — the artifact left by whoever set up native routing — gates one short confirming TCP dial; machines with only a default route pay nothing and port-forward as before. `--direct` forces it (skips the route check), `SLCTL_DIRECT_POD_IPS=always\|never\|auto` overrides detection (e.g. `never` in odd overlapping-CIDR setups, `always` behind a router that holds the pod-CIDR route). **Endpoint overrides (dual-homed/lab clusters where the auto-picked NodePort node IP is unreachable):** `--port-forward` forces a port-forward to pod-0 even when a NodePort/LB Service exists; `--node-ip <ip>` overrides the node address used for a NodePort endpoint (mirrors e2e `E2E_NODE_ACCESS_IP`). **Transparency:** `--verbose` prints the reproducible `kubectl port-forward` + `ldap*` command line so you can run it by hand ("disenchant the magic"); the real password is shown by default (labs — not our job to patronize), `--redact-password` masks it for demos/screenshares. **Reading an accesslog:** a log DB (`cn=accesslog-<database>`) grants read to `cn=replication,<suffix>` and nothing else, with `cn=admin,cn=config` as its rootDN (ADR-020), so `--as config` and `--as replication` can read it while the default `--as admin` — the *data* rootDN — is **denied**; that is a different database with a different rootDN, not a bug. Sibling commands: `ldapadd`, `ldapmodify`, `ldapdelete` (same flags, read LDIF from stdin or `-f`) |

Common flags: `--context`, `--kubeconfig`, `-n namespace`, `--json`, `-A` (all namespaces).

Example after a failed e2e test:
```bash
slctl inspect -n slaptain-testing slapd
slctl debug-dump -n slaptain-testing slapd
```

---

### Reconcile Loop Debugging

`docs/reconcile-loop-fixes.md` is a log of bugs found and fixed in the operator's reconciliation
loop. When debugging reconcile loop issues, **check this file first** — the issue may match a
previously fixed pattern. After fixing a new reconcile loop bug, **update the log** with the
symptom, root cause, why it was hard to spot, and the lesson learned.

---

### Credential & Password Architecture

**Design principle:** The operator manages Kubernetes objects. When it needs to interact with
slapd directly (monitoring, topology changes), it connects over the network using go-ldap with
credentials from a Secret — the same pattern as every database operator (Percona, CloudNativePG,
Strimzi). No kubectl-exec, no sidecar indirection.

**Why not SASL EXTERNAL over ldapi socket?** The ldapi socket lives inside the slapd pod's
filesystem namespace. The operator is a separate pod and cannot access it. A sidecar sharing the
socket was considered but rejected: it trades a narrow RBAC permission (read one Secret) for a
broader one (pod exec or an unauthenticated network endpoint), which is strictly worse from a
security perspective.

**OpenLDAP credential types:**

| Credential | What it controls | Stored as |
|---|---|---|
| Config admin (`cn=admin,cn=config`) | `cn=config` database: schemas, ACLs, replication topology, TLS settings | SSHA hash in `cn=config` |
| Data admin (`cn=admin,<suffix>`) | Data tree: full bypass of ACLs on the data database | SSHA hash in data DB config |

These are independent. Config admin access does not inherently grant data access (though it can
change ACLs to open it up).

**Credential requirements per phase:**

| Consumer | What it needs | Format | Phase |
|---|---|---|---|
| slapd-init (init container) | Config admin password | Plaintext (hashed at runtime by `slappasswd`) | 1+ |
| SlapdDatabase controller: bootstrap | Data admin password | Plaintext | Implemented |
| SlapdDatabase controller: ACL management | Config admin password | Plaintext | Implemented |
| SlapdSchema controller: schema management | Config admin password | Plaintext | Implemented |
| SlapdDatabase controller: replication | Config admin password | Plaintext | Implemented |
| Operator: CSN lag monitoring | Read-only access to `contextCSN` / `cn=monitor` | Plaintext (monitoring DN) or anonymous | Future |
| Consumer init: syncrepl bind | Replication bind password | Plaintext | Implemented |

**Secrets layout:**

| Secret | Contents | Created by | Scope |
|---|---|---|---|
| `<name>-config-password` | `root-password` (plaintext, cn=config admin) | SlapdCluster controller (auto-generated) or user (`spec.ldap.cnConfigCredentials.secretName`) | Operator SA: schema, ACL, replication, and topology management on cn=config |
| `<dbname>-credentials` | `root-password` + `replication-password` (plaintext, per-database) | SlapdDatabase controller (auto-generated) | Init container + operator: data DB bootstrap, syncrepl bind |
| `slapd-test-passwords` | `readpw-<user>` (plaintext, one key per readpw account) | Standalone Secret in `tests/resources/` | Testing only; e2e tests bind as readpw users to verify ACLs |

**Production scope:** The operator provides a correctly configured, healthy LDAP endpoint.
Directory content (OUs, users) beyond what SlapdDatabase seeds is the user's responsibility.

**OpenLDAP replication vs Redis — why the operator is simpler:**
- Delta-syncrepl is pull-based: consumers connect to the provider, provider has no consumer registry.
- No built-in leader election or failover protocol (unlike Redis Sentinel).
- Topology is static configuration in `cn=config`, not a dynamic protocol.
- Adding a consumer = configure it on the consumer side; provider needs no changes.
- The operator's role is configuration management and monitoring, not real-time failover coordination.

---

### Phase 2: Multi-Replica N-way Multi-Master Delta-Syncrepl (implemented)

#### Architecture Decision

**N-way multi-master (mirrormode), not single-provider + dynamic leader election.**

All replicas are symmetric peers: every pod is simultaneously a syncrepl provider AND consumer.
There is no permanent "primary." This design was chosen over "elect pod-0 as provider" because:

1. Pod-0 failure is handled gracefully — no pod is special at runtime.
2. No runtime topology reconfiguration when a pod fails or recovers.
3. StatefulSet's built-in ordered rollout provides bootstrap sequencing for free — no custom
   leader election mechanism needed.
4. OpenLDAP mirrormode is the standard production HA configuration; it handles concurrent writes
   via CSN-based conflict resolution.

"Proper leader election" in the original decision means: *do not hardcode pod-0 as the permanent
provider*. N-way multi-master achieves this without a Kubernetes Lease or sidecar agent.

#### Per-Replica Components (when replication enabled)

Each pod has three databases and two overlays:

| Component | Path | Purpose |
|---|---|---|
| Data DB (`olcDatabase={1}mdb`) | `/data` (existing) | LDAP data; unchanged from Phase 1 |
| Accesslog DB, one per replicated data DB (`cn=accesslog-<dbname>`) | `/accesslog/<dbname>` | Delta-syncrepl change journal, indexed `entryCSN,objectClass,reqEnd,reqResult,reqStart,reqDN eq` (converged on every reconcile). **Never shared between databases** — ADR-019 |
| `overlay accesslog` on data DB | — | Writes every change to *that database's own* accesslog DB |
| `overlay syncprov` on each accesslog DB | — | Exposes that change journal to peers |
| `overlay syncprov` on data DB | — | Required for initial full sync |
| `olcMirrorMode: TRUE` on data DB | — | Enables N-way multi-master writes |
| `olcSyncrepl` (N-1 entries) | — | One syncrepl entry per peer pod |

#### Bootstrap Sequencing

StatefulSet ordered rollout (`podManagementPolicy: OrderedReady`, the default) provides the
necessary ordering at no extra cost:

- Pod-0 starts first; init container bootstraps standalone config, then adds accesslog, syncprov,
  and syncrepl entries pointing to all peers (pods 1..N-1). Those connections fail until peers
  exist — delta-syncrepl retries silently.
- Pod-1 starts after pod-0 is Ready; connects to pod-0, performs initial full sync, then becomes
  a fully equal replica.
- Pod-N-1 starts after pod-N-2 is Ready; same pattern.

After all pods are up, every pod replicates from every other pod. No pod retains special status.

#### PVC Changes (breaking change from Phase 1)

Phase 1 used **shared named PVCs** (`<name>-config`, `<name>-data`) — only works for 1 replica.
Phase 2 switches to **StatefulSet `volumeClaimTemplates`** for per-pod PVCs:

| Template name | Resulting PVC for pod N | Path |
|---|---|---|
| `config` | `config-<name>-<N>` | `/config` |
| `data` | `data-<name>-<N>` | `/data` |
| `accesslog` | `accesslog-<name>-<N>` | `/accesslog` (**new**); per-database journals live in `/accesslog/<dbname>` |

The `reconcilePVCs` controller step is **removed**; PVC lifecycle is managed entirely by the
StatefulSet. Breaking change for Phase 1 deployments (PVC names change). Acceptable at v1alpha1.

#### Replication Credentials

The replication bind password is stored in the `<dbname>-credentials` Secret alongside the
data admin password (per-database):

| Secret | Key | Used by |
|---|---|---|
| `<dbname>-credentials` | `replication-password` | Init container: syncrepl bind password; SlapdDatabase controller: adds `cn=replication` LDAP entry |

The SlapdDatabase controller generates a random `replication-password` when creating
`<dbname>-credentials`. It never rewrites it — but since ADR-027 the Secret is the single
source of truth and **you may rotate it**: change the key and the operator converges the
node-local identity entry and every stanza's `credentials=` on every pod (worst case is the SlapdDatabase resync floor of 5 minutes — a Secret is not a
watched object, so nothing reacts to it sooner; measured 21 s on a four-pod lab
where a tick happened to be pending). See the caveat below before doing so mid-migration.

**Replication bind DN: `cn=repl-<dbname>,cn=slaptain-auth`** (ADR-027) — an entry in a
per-pod, never-replicated authentication database, written and *converged* by the
SlapdDatabase controller from the Secret. Every syncrepl stanza (in-cluster, external and
read-only), the operator's CSN monitoring, the backup controller's source-CSN read and the
ADR-025 suffix probe bind as it.

The pre-ADR-027 identity `cn=replication,<suffix>` — an entry in the *replicated data tree* —
is still created and still granted read (`olcAccess`) and unlimited `olcLimits` on the data
database and its journal. That is deliberate and temporary: it is what keeps a not-yet-upgraded
peer replicating through a staged mesh upgrade (ADR-027 migration steps 2+3). Removing the
grants is step 4, a separate release; the entry itself is then inert and is a documented
manual cleanup, because deleting it is a write into replicated state the operator does not own
(ADR-026 R2).

Two consequences of that split worth knowing before you touch it:

- **The migration window is one-directional.** An old consumer binding to a new provider works
  (the grants are additive). A *new* consumer binding to an *old* provider does not — a stanza
  carries one `binddn` and the node-local entry does not exist there. In-cluster this window
  does not exist; cross-site, pin `externalPeers[].bindDN` to
  `cn=replication,<remote suffix>` until the peer upgrades.
- **Do not rotate the Secret during that window.** `cn=replication,<suffix>` is create-only,
  so a rotation moves the node-local identity and leaves the legacy entry stale — verified
  live, `err=49`. Every upgraded consumer is fine; a not-yet-upgraded one is locked out until
  the legacy entry is repaired by hand.

#### Operator Changes for Phase 2

1. **Remove `replicas > 1` guard** (controller step 2 in Phase 1).
2. **Remove `reconcilePVCs`**: replaced by StatefulSet `volumeClaimTemplates`.
3. **Update `buildStatefulSetSpec`**:
   - Add `accesslog` volume mount to init and main containers.
   - Move config/data/accesslog to `volumeClaimTemplates`.
   - Pass to init container: `LDAP_REPLICATION_ENABLED`, `LDAP_REPLICAS`,
     `LDAP_CLUSTER_HEADLESS_SVC` (`<name>-headless`), `DATABASE_DIRS`, and `LDAP_REPLICATION_PASSWORD`
     (from `<dbname>-credentials` / `replication-password`).

#### slapd-init Changes for Phase 2

Post-ADR-004, the init container handles **cn=config infrastructure only**. Anything that
lives in the data tree or references a specific data database is the SlapdDatabase
controller's responsibility.

Init container, gated on `LDAP_REPLICATION_ENABLED=true`:
- Load `accesslog` and `syncprov` modules.
- Create the per-database accesslog backing directories `/accesslog/<dbname>` from `DATABASE_DIRS`
  (`back-mdb` does not create `olcDbDirectory`). The log **databases** themselves, their syncprov
  overlay and their ACLs are the SlapdDatabase controller's job — see ADR-019 and ADR-020.
- Skip accesslog setup entirely when `LDAP_READONLY_REPLICA=true` (RO pods don't produce changes).

SlapdDatabase controller, per data database, when `spec.replication.deltaSync=true`:
- Add `overlay accesslog` and `overlay syncprov` to the data DB (`ensureReplicationOverlays`).
- Set `olcMultiProvider: TRUE` on the data DB (the OL 2.5+ rename of `olcMirrorMode`).
- Write N-1 in-cluster `olcSyncRepl` stanzas plus external peer stanzas (see ADR-003).
- Converge `cn=repl-<dbname>,cn=slaptain-auth` in the node-local auth database on every pod
  (`reconcileAuthIdentity`, ADR-027) — and **withhold the stanzas above** until every RW pod
  has it, since a provider lacking the entry rejects every consumer that binds as it.
- Still add the legacy `cn=replication,<suffix>` entry to the data tree
  (`ensureReplicationUser`) for the additive migration window.
- Prepend one ACL granting BOTH identities read-all (see reconcile-loop-fixes.md:
  "User ACLs block userPassword replication"), plus the matching `olcLimits` exemption for
  each — a granted identity without its own limits value caps at slapd's default 500 entries
  (ADR-020 amendment).

Peer URL template: `ldaps://<name>-<ordinal>.<headless-svc>.<ns>.svc.cluster.local:1025`

Pod ordinal is read from the hostname: `${HOSTNAME##*-}` (last segment of StatefulSet pod name).

RID scheme (lives on SlapdDatabase.spec.replication.ridBase): in-cluster peer `i` →
`ridBase+i+1`; external peer `j` → `ridBase+50+j+1`. Each database gets a unique ridBase
so stanzas don't collide across databases.

#### Phase 2 Scope and Non-Goals

| In scope | Out of scope (Phase 3+) |
|---|---|
| Replicas > 1 with N-way multi-master | Changing replicas after cluster creation (scale-out) |
| Graceful pod failure and restart | CSN lag monitoring and alerting |
| Per-database replication credentials Secret | |
| Per-pod accesslog PVC | |
| Cross-cluster replication (ExternalPeers, Phase 3) | |
| ACLs on SlapdDatabase CRs (applied per-pod by SlapdDatabase controller) | |
| Schemas on SlapdSchema CRs (applied per-pod by SlapdSchema controller) | |
| Read-only consumer replicas (`spec.readReplicas`) | |

#### Read-Only Replicas

When `spec.readReplicas > 0` (requires `replication.enabled=true`), the operator creates a
second StatefulSet `<name>-readonly` with pure consumer pods. These pods:

- Consume from **all RW masters** via delta-syncrepl (no single point of failure).
- Do **not** run `accesslog`, `syncprov`, or `mirrormode` — they never accept writes.
- Have their own headless service (`<name>-readonly-headless`) and ClusterIP service (`<name>-readonly`).
- Use label `app.kubernetes.io/instance: <name>-readonly` to separate from RW pods.
- Have no accesslog volume or PVC (only `config` + `data`).
- The init container receives `LDAP_READONLY_REPLICA=true`; `LDAP_REPLICAS` and `LDAP_CLUSTER_HEADLESS_SVC` point to the **RW** headless service.
- ACLs are applied to RO pods the same way as RW pods (cn=config is node-local).
- Status fields: `readOnlyReadyReplicas`, `readOnlyReplicas` (informational; do not affect phase).

---

### cn=config Architecture Note

`cn=config` is **node-local** — it is never replicated between pods. Each pod has its own
independent copy stored in the `/config` PVC. This affects anything that lives in
`cn=config`: ACLs (`olcAccess`), schemas, overlays, and syncrepl stanzas.

The operator manages cn=config state by connecting to each pod individually via the headless
service DNS (`<name>-<N>.<name>-headless.<ns>.svc.cluster.local`). ACLs are declared on
`SlapdDatabase` CRs and applied to every pod by the SlapdDatabase controller. Schemas are
declared on `SlapdSchema` CRs and applied by the SlapdSchema controller. Both use the same
per-pod pattern: connect via headless DNS, compare current state, modify if different. See
ADR-002 and ADR-004.

---

### Backlog

Active engineering work is tracked in `docs/MIGRATION-PLAN.md` — a three-phase plan
to replace a legacy OpenLDAP-on-VMs deployment with slaptain. Phase 0 work is
generic and decoupled from the migration timing; Phases 1 and 2 are cold and hot
migration preparation respectively. ADRs 009–011 codify the architectural
decisions referenced from the plan.

The S3 backup/restore feature (ADR-014) has its own breakdown in
`docs/BACKUP-PLAN.md`. Cross-cutting tech-debt not tied to a plan or ADR (e.g.
deprecation cleanups) lives in `docs/BACKLOG.md`.

If you are picking this up after a gap, start with `docs/MIGRATION-PLAN.md` §"Resuming this work".
