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
- [x] **`cn=Monitor` on every pod** (ADR-033 Surface 3, milestone S3-a, 2026-10-08): operator-converged monitor database with a constant ACL. e2e green on a three-site mesh.
- [x] **Exporter sidecar** (ADR-033 Surface 3, S3-c, 2026-10-09): `spec.monitoring.enabled` (opt-in) adds the `slapd-exporter` sidecar — our own generic slapd exporter, curated and correctly typed `openldap_*` families (counters `_total`, gauges as gauges, `openldap_up`) plus `openldap_mdb_*` LMDB fill (ADR-033 decision 4, revised 2026-10-11: upstream/mixin-compatible names withdrawn) — on every RW and RO pod, SASL EXTERNAL over ldapi, no credential. Its own Go module (`exporter/`), image (`slapd-exporter`) and version line (git tags `exporter/vX.Y.Z`, ADR-033 decision 4 "Packaging", 2026-10-11): the operator defaults `spec.images.exporter` to a compiled-in exporter release (`exporterDefaultVersion`, 0.0.1) with the repository derived like slapd/slapd-init, and passes no command or arguments, so an operator upgrade rolls nothing through the sidecar. Opt-in rolls each StatefulSet exactly once; full mesh suite 87/0 with it on. Scraped site-locally by an opt-in PodMonitor in charts/operator (`slapdPodMonitor.enabled`, S3-d, live on three sites 2026-10-09).
- [x] **S3 backup/restore** (ADR-014): `SlapdBackup` (on-demand) + `SlapdScheduledBackup` (cron + retention) → gzipped `slapcat` LDIF to S3 via co-located Jobs; `SlapdDatabase.spec.bootstrapFrom` restores into a fresh DB via a cluster-coordinated scale-to-0 → offline `slapadd` → scale-up machine. e2e green on t3e (versitygw S3 target). See `docs/BACKUP.md`.

---

### Repository Layout

```
.
├── Makefile                        # Root build targets (see Makefile Targets below)
├── .github/workflows/              # CI: test / lint / codeql / security on every push; publish-main (sha-<hash> images
│                                   # on main) and publish (v* tag → images, charts, slctl binaries, CRD bundle, release)
├── hack/verify-generated.sh        # The generated-files drift check behind `make verify-generated`
├── lab.yaml.sample                 # Lab config schema for tests/e2e.sh (real lab.yaml is gitignored)
├── CLAUDE.md
├── docs/
│   ├── BOOTSTRAP.md                # Cluster bootstrap internals (init container + operator phases)
│   ├── DEVELOPMENT.md              # Dev guide: prerequisites, builds, operator loop, e2e cycle, debugging
│   ├── ONBOARDING.md               # Team onboarding: LDAP concepts, operator model, credential model
│   ├── demo/                       # Copy-paste walkthrough (deploy, scale-up, backup, restore) + its manifests
│   ├── MIGRATION-PLAN.md           # Phased plan for replacing a legacy OpenLDAP with slaptain
│   ├── MIGRATION-LEGACY-SOURCE.md  # Source-side (legacy slapd) prep for hot migration
│   ├── BACKUP.md                   # S3 backup/restore user guide (ADR-014)
│   ├── MONITORING.md               # What slaptain exposes to Prometheus (both surfaces), the measured join keys, and what it still cannot tell you (ADR-033)
│   ├── VERSIONING.md               # Where the `v` goes: git tags only; images/charts/appVersion bare
│   ├── TLS.md                      # Certificates: where they come from, and how clients come to trust them (ADR-029)
│   ├── TUNING.md                   # Tuning & sizing guide: defaults, placement classes, lab→prod sizing (ADR-024)
│   ├── DEBUG-IMAGES.md             # slapd:<tag>-debug / -asan: diagnostic image variants for heap corruption
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
│       ├── adr-029-tls-on-by-default-and-legible-certs.md
│       ├── adr-030-readiness-means-slapd-answers.md
│       ├── adr-031-rid-coordinates.md
│       ├── adr-032-cross-site-link-count.md
│       └── adr-033-three-monitoring-surfaces.md
├── charts/
│   ├── operator/                   # Helm chart for deploying the operator itself
│   │   ├── crds/                   # CRD YAML (synced from operator/config/crd/bases/ via make operator-manifests)
│   │   └── templates/              # deployment, RBAC, serviceaccount, metrics, networkpolicy
│   ├── slapd/                      # Helm chart deploying a SlapdCluster CR (operator required),
│   │                               # plus optional `databases:`/`schemas:` (empty by default)
│   ├── slapd-mesh/                 # Helm chart deploying a whole multi-site mesh: SlapdMesh + SlapdCluster
│   │                               # + databases + schemas, applied IDENTICALLY at every site (ADR-028)
│   └── slapd-toolkit/              # Persistent debug pod (ldap-utils, python3, ldap3) wired to operator-managed Secrets
├── images/
│   ├── openldap-deb/               # Vendored Debian packaging fork → OpenLDAP 2.7.1 .debs (ADR-021)
│   ├── slapd/Containerfile         # slapd runtime image (OpenLDAP 2.7.1; Containerfile.ol26 = legacy 2.6)
│   ├── slapd-init/Containerfile    # Bootstrap init container image (2.7.1; Containerfile.ol26 = legacy 2.6)
│   ├── slapd-toolkit/Containerfile # Toolkit image (ldap-utils, python3, pyyaml, ldap3)
│   ├── slapd-exporter/Containerfile # Exporter sidecar image (static binary, distroless/static; own version line)
│   └── operator/Containerfile      # Operator image (multi-stage, distroless/static; /manager only)
├── scripts/                        # Mesh bootstrap, all reading lab.yaml, all with --dry-run
│   ├── mesh-derive-topology.sh     # lab.yaml → the chart's mesh: block (pure; stdout)
│   ├── mesh-share-credentials.sh   # one credential set per database, IDENTICAL per site (ADR-008)
│   ├── mesh-establish-trust.sh     # a cert per site, then every site's CA to the others (ADR-007)
│   ├── mesh-authorize-peers.sh     # RBAC + one kubeconfig Secret per ordered site pair
│   └── image-tag.sh                # the one tag derivation (main `v*` and `exporter/v*` streams), shared by Makefile and e2e.sh
├── monitoring/                     # Monitoring deployment inputs — off-the-shelf only (ADR-033 Surface 1)
│   ├── kube-state-metrics-values.yaml      # CRS config; applied IDENTICALLY at every site
│   ├── kube-prometheus-stack-values.yaml   # applied at EVERY site; that site's Prometheus scrapes only that site (ADR-033 2026-10-09)
│   ├── prometheus-rules.yaml               # alert rules; fuses differ per condition REASON
│   ├── gateway.yaml                        # Gateway API front door — ONE Gateway PER APP (a merged
│   │                                       # catch-all route splits requests across both backends)
│   ├── dashboards/slaptain-mesh.json       # Grafana dashboard (the JSON is the source of truth)
│   └── demo/                               # reversible fault injection; the parameter is substituted
│       ├── blackout.yaml                   # a site dark to inbound LDAP incl. its own operator (VERIFIED)
│       └── cut-inbound.yaml                # one-directional cut via CiliumNetworkPolicy (unverified)
├── exporter/                       # slapd exporter sidecar (own Go module: cmd/slapd-exporter, internal/slapdexporter) — knows slapd, never slaptain (ADR-033)
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
        ├── dataloss_recovery_test.go # Pod loses its PVCs; replication restores DIT (ADR-012 case 2) + ITS#9398 no-storm assertion — **the gating claim here is stale**: it reads as if the storm were `-ol26`-only, and the ITS#9398 investigation established the bug is fixed in no release including 2.7.1 and master. Measured red on 2.7.1 at M5: 4563 stale-cookie lines on one pod after a PVC-loss recovery, limit 50. See `docs/ITS-9398-HANDLING.md`; the spec's own failure message carries the same stale premise
        ├── migration_test.go       # Migration scenario (gated at registration time: E2E_MIGRATION=1)
        ├── external_replication_test.go  # Cross-cluster replication (gated: E2E_EXTERNAL_REPL=1)
        ├── backup_test.go          # SlapdBackup → S3 round-trip (gated: E2E_BACKUP=1, deploys versitygw)
        ├── restore_test.go         # bootstrapFrom restore into a fresh cluster + glue-artifact preflight rejection (ADR-025) (gated: E2E_BACKUP=1)
        ├── restore_replay_test.go  # in-place restore under replication: marker written to the backup's SOURCE pod, asserted present in the artifact + source-honesty status; stale accesslog delete must not replay (gated: E2E_BACKUP=1)
        ├── accesslog_test.go        # Per-database accesslog: structure, no cross-DB lost-sync, convergence, ADR-020 ACL
        ├── sessionlog_test.go       # ADR-022: olcSpSessionlog on every RW pod's data DB; none on accesslog DBs
        ├── monitor_sidecar_test.go   # ADR-033 S3-c: `exporter` container Running + Ready on every RW/RO pod; /metrics via the API-server pod proxy carries `openldap_up 1`, the exporter counters, one representative per curated family group, no `dn`-labelled upstream family, and openldap_mdb_* for every data (and, RW, accesslog) suffix (Labels monitor-sidecar, monitor; ungated; the fixture enables monitoring unless E2E_MONITORING=0)
        ├── tunables_test.go         # ADR-024 degrades/hygiene tunables: cn=config posture per pod (ungated); + Label "monitor" (ADR-033 S3-a): monitor DB on every pod RW+RO, node-local readers only, olmMDB* per mdb (red by design on an upgraded cluster until its pods restart), zero writes at rest (`monitor-at-rest`, 90 s)
        ├── scale_topology_test.go   # spec.replicas up/down must rewrite olcSyncRepl on every surviving pod WITHOUT replacing one: STS revision + pod UIDs unchanged, stanza count +/-1 (ungated, Label "scale-topology")
        ├── cleanup_policy_test.go   # ADR-005 cleanupPolicy: Delete tears a replicated DB (+ its accesslog DB) off EVERY pod; Retain leaves it; re-adoption (ungated, Label "cleanup-policy"; rolls the cluster per ADR-013)
        ├── scale_test.go            # many-entries fixture: 1200-entry seed + churn (gated: E2E_SCALE=1; knobs E2E_SCALE_ENTRIES/E2E_SCALE_CHURN)
        └── scaleup_test.go         # standalone → HA transition: schema/modules/serverID runtime convergence (gated: E2E_SCALEUP=1)
```

---

### Image Details

- **Build tooling:** Plain Containerfile + podman (apko dropped — only beneficial in the Wolfi ecosystem).
- **Debug variants** (`docs/DEBUG-IMAGES.md`): `BUILD_VARIANT=release|debug|asan` on both the package build and the slapd runtime Containerfile. `-debug` ships **bit-identical** binaries plus their `-dbgsym` debug info, so it resolves offsets recorded against the release image of the same tag; `-asan` is an AddressSanitizer build that names an invalid free or use-after-free at the moment it happens. libasan's in-process symbolizer reads `/usr/lib/debug/.build-id`, which is why no external symbolizer is needed in a distroless image. `nostrip` is NOT the route — `dh` drops `dh_strip` under it and this packaging creates the `slapadd`/`slappasswd` hardlinks inside `override_dh_strip`.
- **slapd runtime** (`images/slapd/`): OpenLDAP **2.7.1** (`2.7.1-0+slaptain1`, built from the vendored packaging fork in `images/openldap-deb/` — ADR-021) on `gcr.io/distroless/base-debian13` — glibc, libssl, ca-certs, no shell. Legacy Debian 2.6 build stays available as `slapd:<tag>-ol26` (`Containerfile.ol26`) for hot-migration interop (ADR-011). **LMDB 1.0 format break:** 2.6-written volumes cannot be opened by 2.7 — see `docs/OPENLDAP-VERSIONS.md` for the migration runbook.
- **slapd-init** (`images/slapd-init/`): `debian:trixie-slim` + the same locally-built 2.7.1 packages (slapcat/slapadd/slaptest must match the slapd version) — ephemeral bootstrap; shell + python3. `-ol26` variant pairs with the 2.6 slapd image — always pin both or neither.
- **operator** (`images/operator/`): `gcr.io/distroless/static-debian13:nonroot`, statically-linked Go binary (`/manager` only), UID 65532. Builder stage uses `golang:1.26`.
- **slapd-exporter** (`images/slapd-exporter/`): `gcr.io/distroless/static-debian13:nonroot`, static binary, `ENTRYPOINT ["/slapd-exporter"]`, built from the `exporter/` module. Its own version line: git tag `exporter/vX.Y.Z` → image `X.Y.Z` (`docs/VERSIONING.md`); the operator pins a compiled-in default.
- **User (slapd):** `openldap` (UID/GID 1024). Debian's slapd package creates this user; we `groupmod`/`usermod` to 1024.
- **Ports:** 1024 (ldap), 1025 (ldaps) — non-privileged. Service maps 389→1024 and 636→1025.
- **Probe client:** the runtime image carries exactly one LDAP client, `/usr/bin/ldapwhoami`, for the readiness/startup exec probes (ADR-030). Same vendored package build as slapd, so it can never drift from the server. Both image variants carry it — a `-ol26` pod without it would never go Ready. Debugging still belongs to `slapd-toolkit`.
- **Mount Points:** `/config` (slapd.d config dir, PVC), `/data` (LMDB data, PVC), `/accesslog` (delta-syncrepl change journals, PVC — one `<dbname>` subdirectory per replicated database, ADR-019), `/run/openldap` (socket, emptyDir), `/etc/openldap/tls` (TLS secret, optional).
- **Module path:** `/usr/lib/ldap` (Debian path). Modules loaded dynamically; plan to compile in statically later.
- **Schema path:** `/etc/ldap/schema/` (Debian path).
- **Init Container:** Sets up `cn=config` (admin credentials, modules, TLS). Creates per-database data directories from the `<name>-database-dirs` ConfigMap mounted at `/etc/slaptain/database-dirs` (a ConfigMap, not pod-template env, so a database add/remove rolls nothing — ADR-013 amendment 2026-09-28). Does NOT create data databases, schemas, or ACLs.

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
| `spec.replication.replicasPerPeer` | `*int32` | Cluster-wide default for the above, and **the only form a mesh cluster can reach** — `externalPeers` is derived under `meshRef` and refused if set. Stamped onto every derived peer; the per-peer field still wins where set. Buys cross-site link redundancy (a second route when the remote pod at the far end goes away), not coverage. Added 2026-09-28; the gap was missed, not decided — ADR-028 amendment |

Database-level config (ACLs, schemas, indices, replication per-DB) is declared on `SlapdDatabase` and `SlapdSchema` CRs. Per-DB tunables (`maxSize` — recreate-only, slapd segfaults on live modify; `sizeLimit`/`timeLimit`/`limits` default unlimited; checkpoint, `noSync`, `envFlags`, `accesslogPurge` default `7+00:00 1+00:00` with `"none"` opt-out, `syncprovCheckpoint`) follow ADR-024's placement classes — see `docs/TUNING.md`.

**Status fields:** `phase` (Bootstrapping/Running/Degraded/Error/**Restoring**), `readyReplicas`, `replicas`, `readOnlyReadyReplicas`, `readOnlyReplicas`, `observedGeneration`, `replicationNetworkIPs` (discovered Multus IPs per pod), `externalPeerStatuses` (per-peer: `replicationState` Synced/Lagging/**PartiallyVerified**/Unreachable, `lagSeconds`, `lastChecked`, `discoveredProviders` — each a remote pod's StatefulSet `ordinal` plus the `address` it currently answers on, ordinal-ordered, ADR-031), `restore` (in-progress bootstrapFrom restore: sub-`phase`, `originalReplicas`, `databases` — ADR-014), `conditions` (including `ReplicationConverged` for local CSN convergence and `PodsResponsive`, which names any read-write pod that answered no LDAP at all this pass — pure observability, ADR-030; its `lastTransitionTime` is how long the pod has been silent).

**CSN convergence is judged per database** (ADR-008 amendment 2026-09-14). A `contextCSN`
vector belongs to one database on one pod and is only comparable to the same database's
vector elsewhere; `ReplicationConverged` is the AND over databases (`CSNsMatch` /
`CSNsDiverged` naming the database / `CSNQueriesIncomplete` when a pod×database pair could
not be read — unreadable evidence never counts toward True). `PartiallyVerified` is the
cross-site counterpart: everything comparable is current, but at least one database yielded
no readable contextCSN on one side of the comparison (`lastError` names the databases).
**Either side** — a missing LOCAL baseline is the same absence as a peer that would not
answer, and it used to read `Synced` because a zero baseline made every remote CSN look
ahead and the remote-ahead clamp zeroed the lag (ADR-008 amendment 2026-10-06). The one
asymmetry is deliberate: a peer answering with no CSN while we hold a baseline is a measured
deficit and stays `Lagging`. The peer check is no longer skipped when local reads fail, so a
site can no longer publish `connected: false` for a peer it never contacted.

**Reconcile order (SlapdCluster controller):**
1. Fetch `SlapdCluster` — NotFound → return nil (deleted)
2. `reconcileSecret` — create `<name>-config-password` (plaintext `root-password`, create-only); or read from `spec.ldap.cnConfigCredentials.secretName`
3. `reconcileHeadlessService` — `<name>-headless`, `clusterIP: None` (SSA patch)
4. `reconcileClusterIPService` — `<name>` (bare name), ClusterIP (SSA patch)
4a. `reconcileDatabaseDirsConfigMap` — `<name>-database-dirs`, the per-database LMDB directory lists (`data-dirs` + `accesslog-dirs`) the init container reads at pod start (SSA patch)
5. `reconcileStatefulSet` — `serviceName: <name>-headless`; mounts the directory-list ConfigMap into the init container **by name**, so the pod template is invariant under the database set; provisions `volumeClaimTemplates` for config/data/accesslog (persistence is mandatory per ADR-013) (SSA patch)
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

**Owned resources:** StatefulSet, Service (×2), Secret, ConfigMap (`<name>-database-dirs`), PersistentVolumeClaim — all get `SetControllerReference`. The SlapdDatabase controller additionally owns the short-lived mkdir Jobs (`<pod>-mkdir-<hash>`), reaped per ADR-018.

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
logical site `site-N` at `serverIDIndex` N−1, so serverID bases are 0/16/32 and
pods read `001`../`011`../`021`.. in a CSN (hex stride, ADR-028 amendment
2026-10-05; the fixture is recreated every run, so it simply adopted it). The hand-wired path — and with it `E2E_MESH`, the peer loop and
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
| `make all` | Build all images (2.7 slapd pair, `-ol26` 2.6 pair, toolkit, operator, slapd-exporter) |
| `make build-init` | Build slapd-init image |
| `make build-slapd` | Build slapd image |
| `make build-operator` | Build operator image (build context = repo root) |
| `make build-exporter` | Build the slapd-exporter image at `EXPORTER_IMAGE_TAG` (its own tag stream) |
| `make push` | Build + push all seven images (both slapd pairs, toolkit, operator at `IMAGE_TAG`; slapd-exporter at `EXPORTER_IMAGE_TAG`) |
| `make show-tag` | Print the git tag and the image/chart versions derived from it (`v0.2.1` → image `0.2.1`, chart `0.2.1`), and the exporter stream's (`exporter/v0.1.0` → image `0.1.0`) |
| `make helm-lint` | Lint all four published charts and render each in the shapes users install (operator with Multus/metrics/NetworkPolicy on, slapd with the persistent fixture values, the mesh from its three-site example). CI's chart gate and the release's pre-push check |
| `make verify-generated` | Regenerate CRDs/RBAC/deepcopy and fail if any checked-in copy (incl. `charts/operator/crds/`) drifts — `hack/verify-generated.sh`. CI's generated-files gate |
| `make build-openldap-deb` | Build the local-only OpenLDAP 2.7.1 .deb carrier image (feeds both 2.7 image builds; never pushed) |
| `make build-slapd-debug` / `build-slapd-asan` | Build a diagnostic slapd variant (`:<tag>-debug` = release-identical binaries + detached debug info + glibc heap checking; `:<tag>-asan` = AddressSanitizer). Each needs its own package build. Deliver with `import-slapd-debug`/`push-slapd-debug` (or `-asan`); deploy by pinning `spec.images.slapd.tag` ALONE — unlike `-ol26`, the init image stays on the plain tag. Never in `all`/`push`, never in a chart. See `docs/DEBUG-IMAGES.md` |
| `make build-ol26` / `make import-ol26` | Build / CRI-import the legacy 2.6 slapd+init pair (`:<tag>-ol26`, ADR-021) |
| `make operator-generate` | Run `make generate` in `operator/` (regenerates deepcopy) |
| `make operator-manifests` | Run `make manifests` in `operator/`, then sync CRD to `charts/operator/crds/` |
| `make operator-sync-crd` | Copy CRD from `operator/config/crd/bases/` to `charts/operator/crds/` |
| `make operator-crd-apply` | `kubectl apply --server-side` the CRDs from `operator/config/crd/bases/` (Helm only installs `crds/` on first `install`, never on `upgrade`) |
| `make operator-helm-install` | `helm upgrade --install slaptain ./charts/operator` (depends on `operator-crd-apply`, so a new/changed CRD lands on upgrade too). t3e loop: `make operator-helm-install CONTEXT=t3e GIT_TAG=<tag>`. `SITE_NAME=<site>` sets the operator's mesh site identity (ADR-028 §4) — needed to walk `docs/MULTI-SITE.md` against a local build, since `make <target> --set …` cannot work (make parses `--set` itself); `HELM_ARGS=…` passes anything else through |
| `make operator-helm-uninstall` | Uninstall the operator Helm release |
| `make gencert` | Generate self-signed TLS cert via `tests/gencert.sh` |
| `make helm-deploy` | Full pipeline: `deliver` + `gencert` + `cluster-helm-install` |
| `make cluster-helm-install` | `helm upgrade --install slapd ./charts/slapd` |
| `make mesh-helm-install` | `helm upgrade --install ldap ./charts/slapd-mesh` at one site, from the working tree — the local-build counterpart of `docs/MULTI-SITE.md` step 7. `MESH_RELEASE`/`MESH_NAMESPACE`/`MESH_VALUES` override the release, namespace and values files; loop it over `CONTEXT` for the whole mesh. Pins no image on purpose: the chart leaves `cluster.images` empty so the operator defaults both data-plane images to its own tag |
| `make mesh-helm-uninstall` | Uninstall that release |
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
| `make monitoring-install` | Site-local monitoring (ADR-033 2026-10-09), run per `CONTEXT`: kube-prometheus-stack (pinned `MONITORING_STACK_VERSION`, + rules + dashboard), then kube-state-metrics (pinned `MONITORING_KSM_VERSION`, ClusterIP + ServiceMonitor). Then the operator with `metrics.enabled`, `metrics.serviceMonitor.enabled`, `slapdPodMonitor.enabled` — for an already-installed operator via `helm upgrade --reuse-values`, never `make operator-helm-install` (it swaps in this checkout's image and rolls every slapd pod). No site scrapes another; there is no `site` label. `monitoring-ksm-install`, `monitoring-stack-install`, `monitoring-rules-apply`, `monitoring-dashboard-apply` and `monitoring-rules-test` (promtool) remain as parts |

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

### Versioning: where the `v` goes

The `v` belongs to the **git tag** and to nothing downstream of it. Images, chart `version`,
chart `appVersion` and `helm --version` all take the bare semver — `v0.2.1` is tagged in git
and published as `operator:0.2.1` / chart `0.2.1`. The Makefile keeps the two questions in two
variables: `GIT_TAG` is what HEAD *is* (version tag, else short hash, else `<hash>-dirty-<state8>`),
`IMAGE_TAG` is how it is *addressed* — bare semver on a version tag, `sha-<hash>` otherwise,
matching what `docker/metadata-action` publishes in the sibling project whose workflows
slaptain plans to adopt. This makes the charts' `image.tag | default .Chart.AppVersion` fallback an
identity rather than a coincidence. Adopted at v0.2.1 (images through `v0.2.0` keep the `v`;
that discontinuity is deliberate and documented). The slapd exporter has a second, independent
stream: git tags `exporter/vX.Y.Z` → image `slapd-exporter:X.Y.Z`; the main stream's lookup matches
`v*` only, so a commit carrying both resolves unambiguously. Full rules, traps and verification recipes:
`docs/VERSIONING.md`.

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

**A mutation check is only evidence if the mutation landed.** Proving a
green-from-birth test's teeth by breaking the code and watching it fail is the
right move, and it has silently failed three times in one effort — each time
reading as "this test has no teeth" when the truth was "the check did not run":
a `str.replace` whose source did not match, a marker comment that produced a
syntax error, and an edit that failed to compile on an unused import. A `grep`
confirming the marker is present proves nothing. **Verify the mutated tree
builds, then verify the test went red for the mutation's own reason** — and
treat a green mutation result as suspicious until you have done both. A test
whose teeth were "proven" by a check that never ran is worse than an untested
one, because it reads as coverage.

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
- ADR-010: SlapdCluster replication modes (peer / consumer-only, in-place promotion) — *Accepted (impl + e2e green 2026-05-13); amended 2026-10-07: `spec.replication.enabled` is a creation-time declaration and a ONE-WAY on-ramp, not a pause switch. `false → true` is live, supported and (since the ADR-013 amendment of the same date) rolls no pod. `true → false` is a half-teardown nothing rejects: the syncprov/accesslog overlays go and the accesslog DB is **dropped**, while `olcSyncRepl` and `olcMultiProvider` are left untouched — step 10 is `if enabled {…}` with no else — leaving every pod consuming from peers that no longer serve syncrepl. The designed demotion is `mode: consumer-only`, which keeps `enabled: true` so the stanza set CONVERGES instead of being abandoned. A real disable path is deliberately unbuilt: it would be the destructive short-set `Replace` that ADR-031 decision 6 exists to prevent, on state ADR-026 R2 says not to destroy. Observability item filed in `docs/BACKLOG.md`*
- ADR-011: Hot migration topology contract (RID/ServerID coexistence, plain-syncrepl interop, stage transitions) — *Accepted (impl + e2e green 2026-05-13)*
- ADR-012: Seed is one-shot; cluster wipe is a Kubernetes resource lifecycle operation (replaces removed `forceRebootstrap` + reverted `verifySeedExists`)
- ADR-013: Defer hot SlapdDatabase add/remove; require persistent storage — *amended twice on 2026-09-28: first, `DATABASE_DIRS` is the ONLY sanctioned pod-template roll trigger (an `LDAP_REPLICAS` leak had been rolling the fleet on every scale-up for months); then that exemption is retired too, so the count is **zero**. The directory list moves to a ConfigMap (off the template, so add and remove both stop rolling) and already-running pods get their directories from a co-located mkdir Job — ADR-018's pattern, which postdates the May deferral and makes option H dominate the C'/G/F options this ADR costed. Subsumes a latent defect: the list was keyed on CR names, but a database's data directory is `spec.dataDirectory` when set. Mesh-validated 2026-09-28 (three sites, 96/1 — the one failure was the scale-topology spec's own nil-pointer on a nil `spec.replication.network`, fixed, then 4/4 on re-run). Three defects came out of running it, all absences rather than failures: a duplicate Job per RW pod (two call sites, one pass), a Job orphaned when the path set changed before the reap (the name is a hash of it — TTL removed it at 5m03s), and `slctl inspect` judging syncrepl on the first data DB only; amended a third time 2026-10-07: **the count was not zero, it was two** — `NeedsAccesslogVolume()` (the `/accesslog` mount) and `spec.replication.enabled` (`LDAP_REPLICATION_ENABLED`) were both in the pod template and both flip on the one edit everybody makes, a standalone cluster becoming an HA one. Pods 1..N are born at the new revision, pod-0 is left stale and gets replaced; it reads as `Running` **then** `Degraded` (with `OrderedReady` the new pods all reach Ready before the stale one is touched, so `readyReplicas == spec.replicas` is briefly true and honest), with `restartCount: 0` and pod-0 *younger* than its siblings — the inverted age is the fingerprint. Both effects are now unconditional on every RW pod, which is what `olcServerID` has done since ADR-017; costs one upgrade roll of RW pods (RO pods unaffected, their template is byte-identical). The 1↔2 boundary had been written down as a deliberate exemption in the guard test's own comment* *Amended 2026-10-08: the zero-trigger rule is about side-effect rolls (another resource's lifecycle, scaling, derived state, an operator upgrade); a field whose purpose is to change the pod — an image, `resources`, `logLevel`, `monitoring.enabled` — rolls when the user changes it, and its unset/off state must be byte-identical (pinned by golden pod templates in `exporter_sidecar_test.go`).*
- ADR-014: S3 backup/restore (slapcat→gzip→S3 via co-located Job; bootstrapFrom restore into a fresh DB) — *Accepted (impl + e2e green 2026-06-09; amended 2026-08-24: `SlapdRestore` is a rollback only without external peers — on a mesh member peers replay newer changes, so it is a local re-seed; true rollback is a mesh-wide human runbook)*
- ADR-015: Cluster DNS domain is resolved (CLUSTER_DOMAIN env → resolv.conf → cluster.local), never hardcoded — *Accepted 2026-07-05*
- ADR-016: Direct native pod-IP routing as a cross-cluster replication transport (alongside Multus/NodePort; `network.mode: pod-routed`) — *Accepted (impl + e2e green across three routed-pod-CIDR sites 2026-07-06)* — *amended 2026-08-26: replacing a pod invalidates every peer's syncrepl addresses, including via the scale-to-0 restore machine; measured recovery 130-268s, because address discovery rides on the 60s CSN-monitoring tick*
- ADR-017: `olcServerID` is a bare integer (`serverIDBase + ordinal + 1`), not the URL-list self-match form — sheds the FQDN/cluster-domain coupling that made serverID the ADR-015 boot crash surface — *Accepted 2026-07-17*
- ADR-018: Co-located PVC access — RWO is per-node, but any pod object naming a PVC (finished pods included) is a deletion lease that blocks pod re-roll; the operator reaps every Job it creates — *Accepted 2026-08-23*
- ADR-019: One accesslog DB per replicated data DB (`cn=accesslog-<dbname>` at `/accesslog/<dbname>`) — a shared log makes every write to one DB kick the other DB's consumers into full refresh; verified against slapd sources and upstream guidance — *Accepted (impl + e2e green 2026-08-25; amended twice the same day from the first live run — R8 step order + reference-counted delete, then the "never reuse an olcDatabase={N} DN across a delete" rule and per-pod stanza deferral; Consequences corrected: a missing `logbase` **halts** replication, it does not degrade to full refresh; amended 2026-09-12: adopts upstream's full index set — adds `reqDN`, `entryCSN`, `objectClass`, drops the no-op `default eq` — converged onto existing log DBs; amended 2026-09-14: **R8 withdrawn** — the operator no longer migrates a legacy cluster-shared log. Its reference-counted delete authorised itself from other databases' overlays (state the operator does not own) and `DropOverlay` was gated on the database it deletes, so a stranded `olcAccessLogDB` was unrepairable and fatal at the pod's next startup — slapd validates `logdb` online but resolves it offline in `accesslog_db_open`. Replaced by a manual runbook + an `slctl inspect` issue; the rule is ADR-026)*
- ADR-021: OpenLDAP 2.7.1 by default, built from a vendored Debian packaging fork (`images/openldap-deb/`); the 2.6 pair stays as `:<tag>-ol26` for hot-migration interop (ADR-011). ITS#9580 is fixed in no 2.6.x release and no distro ships 2.7; LMDB 1.0 makes 2.6→2.7 a dump/reload (`docs/OPENLDAP-VERSIONS.md` runbook) — *Accepted (images + single-site e2e green on 2.7.1 2026-09-11; the ITS#9580 storm red and mixed-mesh validation belong to the deferred multi-site session — a fresh single-site cluster measured 0 storm lines even on 2.6)*
- ADR-022: The syncprov sessionlog belongs on the data DB only — never an accesslog DB, where a successful replay would displace the minCSN guard and under-replicate silently. On by default at 5000 ops via `spec.replication.syncprovSessionlog` (tristate: unset→5000, 0→off, >0 verbatim) — *Accepted (unit red-first + e2e red/green on live clusters 2026-09-11)*
- ADR-023: Rolling volume replacement — an imperative, one-shot `SlapdRollingReplace` rebuilds pods from the mesh one ordinal at a time (ADR-012 case 2 promoted to an operation), gated on surviving redundancy and CSN convergence; headline use case: the 2.6 → 2.7 LMDB format break (ADR-021) without the ADR-014 outage — *Proposed 2026-09-12, implementation after v0.1.0*
- ADR-024: Where an OpenLDAP tunable lives — three placement classes (converged-per-pod as the default, bootstrap-time, create-only-by-nature), a spec field that cannot be honoured is rejected or converged but never ignored, and unset means slaptain's default rather than slapd's — *Accepted 2026-09-12; amended twice from live evidence: olcDbMaxSize and olcDbEnvFlags segfault slapd on a live modify, olcIdleTimeout/olcWriteTimeout and the monitor ACL MOD hang it — R1 membership is proven on a running pod; amended 2026-09-21: R1 says WHEN a tunable converges, not into which entry — `olcPasswordHash` moves to `olcDatabase={-1}frontend` because slapd 2.7 deprecates the global entry and warns it "may refuse to start" for module-provided schemes, which is the {ARGON2} case the backlog contemplates. Placement is per attribute and expressed as data, and the operator retires the copy an older version left behind. User-facing guide: docs/TUNING.md*
- ADR-025: Seed is single-creator MESH-wide (founder-only: exactly one site carries `spec.seed`; peers sync). A multi-site seed race demotes one pod's suffix entry to a permanent hidden GLUE — invisible to ordinary searches (ManageDSAIT reveals it), frozen at the winner's entryCSN so all CSN health reads clean, silently breaking that pod and making every backup from it unrestorable. Fix set: e2e founder-only seeding; operator withhold belt (foreign-sid suffix creator → seed withheld — the OPPOSITE direction of the reverted verifySeedExists); restore preflight rejects a glue/RDN-less suffix entry (destroy-last); `SourceSuffixHealthy` on SlapdBackup; `DataPresent` all-pods; `slctl inspect` suffix-visibility + suffix-uuid-agreement; NO auto-heal (manual runbook in the ADR) — *Accepted 2026-09-13; amended 2026-09-14: following Decision 1 switched OFF Decision 5 — a never-seeded database (every non-founder site, migration, consumer-only, bootstrapFrom) never latched `SeedApplied`, so the per-pod suffix probe never ran; measured 4 of 6 databases on a healthy mesh. The probe is now ungated and the seed latch informs only what an all-empty reading means (`Unknown/NoDataYet` vs `False/DataMissing`, via the new one-way `status.dataObserved`); a ManageDsaIT-confirmed glue reports `False/GlueSuffix` on its own. One shared probe in `internal/suffixprobe` for operator, backup and slctl; amended again 2026-09-14: the all-pods rule now includes the **read-only fleet** — an RO-only divergence reports `False/DataMissingOnReadOnlyPods` (its own reason, because an RO initial sync makes it legitimately transient), phase-neutral, byte-identical at `readReplicas: 0`*
- ADR-026: What a `SlapdDatabase` reconcile may do to `cn=config` state it does not exclusively own — R1 `olcDatabase={N}` is a positional namespace, so re-resolve after any delete (implemented, correct); R2 never destroy shared state on evidence the operator does not own (another database's overlays, a hand edit — ADR-002 sanctions both; report and stall instead, because the bad state here is silent until a restart); R3 trigger a repair on the condition it repairs, never on a sibling artifact the same pass may delete. Origin: the withdrawn ADR-019 R8 migration stranded an `olcAccessLogDB` and made a pod permanently unbootable. R2/R3 are forward-looking guards — after R8's removal no live path violates them — *Accepted 2026-09-14*
- ADR-027: The replication identity is node-local, not an entry in the replicated tree — `cn=repl-<db>,cn=slaptain-auth` in a per-pod, never-replicated auth database on the data PVC, converged from the Secret (so rotation finally works). Kills the two-store class at its cause: the duplication is inherent to simple bind, but putting the verifier copy in the *replicated customer tree* is what made it shared multi-writer state (ADR-026 R2) and produced both the 19-day dead db2 link and a password-stripped copy. Fixed infrastructure: outside `DATABASE_DIRS`, so no new ADR-013 roll. SASL EXTERNAL/mTLS considered and deferred with its real marginal value recorded — *Accepted 2026-09-15 for what a single site can show; amended twice: 2026-09-14 (M1, the database + entries, additive) and 2026-09-15 (M2, the cutover — stanzas, ACLs and olcLimits and the operator's CSN bind all name the node-local DN, with the legacy grants RETAINED; identity convergence is now an ordering GATE that withholds syncrepl until every RW pod carries the entry; rotation measured at 52 s across four pods). Two live findings: the migration window is one-directional (a stanza carries one binddn, so a NEW consumer against an OLD provider fails — pin `externalPeers[].bindDN` meanwhile), and rotating the Secret mid-window strands the create-only legacy entry. Mesh-validated 2026-09-15 (86/2; the two failures were the gate firing on TLS-less fixture clusters whose auth database a pre-existing short-circuit never created — `runConvergenceSteps`, see docs/reconcile-loop-fixes.md). Mixed-VERSION mesh behaviour remains reasoned, not measured*
- ADR-028: The mesh is a layer of its own — `SlapdMesh` (sites, network, trust) ← `meshRef` ← `SlapdCluster` (+ a `sites` subset selector) ← `clusterRef` ← `SlapdDatabase`/`SlapdSchema`. A mesh-scoped resource carries **no per-site fields**, so it is byte-identical everywhere and parity is existence + a hash; the single per-site fact is `SITE_NAME` on the operator's own chart, never in a CR. Four of nine cross-site invariants stop existing (the operator derives `serverIDBase` from a **declared, not positional** `serverIDIndex`, plus `externalPeers`, network mode and trust wiring); the rest survive as one chart applied identically. `spec.seed.site` replaces the e2e's seed-stripping, making ADR-025's single-creator rule a property of the spec rather than of a deployment procedure. Packaging unit is a chart, not a fused CR. `meshRef` + an explicitly set derived field is refused, never silently preferred. Operator fan-out to peer clusters rejected (ADR-026 R2) — *Accepted 2026-09-18: all 7 MESH-PLAN phases landed, three-site lab at 88/93 with zero failures, serverID slots reproduced exactly; amended 2026-10-05: the per-site stride moves from decimal 100 to hex 16, because a serverID is only ever read in the three-hex-digit `#sid#` field of a CSN — site 1 pod 0 now reads `011` where it read `065`. 8 bits for the site and 4 for the pod, asymmetric on purpose: pod ordinals are reused by a StatefulSet, site indices are retired forever, so the leaky axis gets the room. Creates one new bound — `ridLayout.podsPerSite` capped at 15, since a site's ordinal 15 would take the next site's base — enforced on the declared layout so `podsPerSite` stays the single bound on `spec.replicas`. Re-striding a LIVE site is still a defect (MESH-PLAN hazard 2); taken while every deployment was a recreatable lab. Deleting the hand-wired e2e path cost the only coverage of NodePort `uri` and static Multus `podAddresses` peers (docs/BACKLOG.md); the mesh `multus` branch is written but untested*
- ADR-029: TLS is on unless the user turns it off, and a missing certificate is a legible state — the **gate** shipped in v0.2.1: when TLS is on, the cluster controller reads the named Secret and withholds only the StatefulSet until it carries `tls.crt`/`tls.key`, reporting `TLSReady=False` with a reason that names the Secret (and never tearing down a running cluster whose Secret was deleted — ADR-027 shape). The **default flip** (`enabled` → `*bool` defaulting to on, `secretName` → `<name>-tls`) is deferred until the operator can issue certificates itself, since flipping it earlier only makes the first run worse. Cluster-root-CA issuing is documented, not recommended: kubelet-csr-approver denies that CSR shape outright (docs/TLS.md) — *gate Accepted 2026-09-19; flip Proposed*
- ADR-030: Ready means slapd **answers LDAP**, and nothing restarts a frozen pod — an exec `ldapwhoami -x` on loopback (WhoAmI reads no entry, so no ACL can break the probe; TCPSocket was disproven by the 874 s freeze, which accepted connections throughout while holding `readyReplicas: 3`). `startupProbe` 300 s so a slow LMDB open cannot prevent a cluster forming under `OrderedReady`; `readinessProbe` 60 s, chosen to sit well above a routine sub-second `cn=config` pause and far below the freeze. **No liveness probe** — restarting a pod mid-freeze is an ADR-016 topology event at every peer; a frozen pod leaves the Service, not the cluster. Headless Services publish NotReady addresses (landed first, alone, while still a no-op), because a NotReady pod that drops out of headless DNS is one the operator cannot converge and ADR-027 would answer by withholding syncrepl site-wide. The convergence question stays deferred where ADR-013 left it; the startup budget is an admitted guess above fixture size, awaiting the big-DIT e2e lane — *Accepted 2026-09-28; mesh-validated the same day (three sites, 98/0/1, nothing turned red — the specs that wait on `readyReplicas` now wait on a metric that can fail and still pass, at ~2% more wall clock). Pods reach Ready in 3-5 s; a pod mid-restart correctly read NotReady and recovered. Behaviour against an actual frozen slapd stays reasoned, not measured — that incident has never been reproduced on demand*
- ADR-033: Monitoring has three surfaces and a metric belongs to the vantage point that observes it DIRECTLY, never to one that would relay it — the API server (CR state, via kube-state-metrics' custom-resource-state, no slaptain code), the operator (cross-pod and cross-site judgments only it performs), the pod (slapd's own counters, over the pod-shared `ldapi` socket with SASL EXTERNAL and no credential — the reason CLAUDE.md gives for rejecting ldapi applies to the operator being a separate pod and does not carry to a sidecar). No quantity is exported twice; the one sanctioned overlap is `up`, because the three fail independently. Unreadable evidence is absent, never `0` (ADR-008 carried onto a surface with no `Unknown`). — *Proposed 2026-10-06; Surface 1 implemented and live on a three-site mesh the same day (docs/MONITORING.md); amended 2026-10-08 (supersedes decision 6): the monitor database is **R1, operator-converged and always present** (no switch — an off-transition would be a renumbering delete), its ACL a **constant** independent of the database set (`dn.children` on `cn=slaptain-auth` and on the peercred base, so the ldapi reader is UID-agnostic — no `olcAuthzRegexp`, nothing pinning UID 1024), and the 2026-09-13 "hang" re-measured as not reproducing (71 live replaces) and most likely the same-day `timeout=` freeze — indicated, not demonstrated. Same-day correction: `olcMonitoring` is on by default and inert live; what decides `olmMDB*` is **create order** — an mdb gets statistics only if the monitor database existed when it was opened, so the SlapdDatabase controller ensures it immediately before every ADD, and pods whose databases predate it need one restart. `back_monitor` is static in every image; nothing loads it. Steady state is one read per pod per pass and zero writes (e2e `monitor-at-rest`). The sidecar sits behind `spec.monitoring.enabled`, default off — opt-in, so an operator upgrade rolls nothing. Amended again 2026-10-08: Surface 3 is **our own exporter**, written against slapd not slaptain (imports nothing from the API, reads no CR), (tomcz/openldap_exporter is archived, imageless and cannot bind EXTERNAL) — revised 2026-10-11: curated, correctly typed families replace the upstream/mixin-compatible names (breaking; a coverage test holds the table to recorded 2.7.1 and `-ol26` dumps), plus `openldap_mdb_*`; collect on scrape, absent on failure, a 5 s socket deadline; its own binary (~16 MiB vs ~55 MiB as a `/manager` subcommand) — since 2026-10-11 its own module, image and version line, the operator defaulting to a compiled-in exporter release and passing no arguments, so an operator upgrade rolls nothing through the sidecar (the introducing release rolls monitoring-enabled clusters once); `/run/openldap` mounted read-only. Live 2026-10-09 on three sites (S3-c3). Amended 2026-10-09: **monitoring is site-local** — each site's Prometheus scrapes only its own ksm, operator and exporter sidecars, no site scrapes another, no Prometheus means no monitoring; the only global view is what the operator and CRs already hold (peer verdicts, exported locally). The central observer and its NodePort scrapes are withdrawn and `site` has no job any more. ServiceMonitors use `honorLabels: true` plus a target-`pod` labeldrop (else ksm/operator `namespace`/`pod` become `exported_*`); rules gain a `slaptain.slapd` group (exporter down, cn=Monitor unreadable (`openldap_up == 0`), dial/bind failing, LMDB fill net of the free list for data and accesslog, reader slots) and the peer joins key on (namespace, cluster, mesh, peer); no thread-saturation rule (Pending>0 measured at 2-9 of 16 active). Live on three sites the same day*
- ADR-020: An accesslog DB is at least as restrictive as the database it journals — *amended 2026-09-12: the replication identity also gets unlimited olcLimits on the data DB and the journal (slapd's default sizelimit of 500 capped every syncrepl search — found when a journal outgrew it live); never modify olcDbMaxSize on a live database (slapd segfaults — ADR-024 amendment)* — `to * by dn.exact="cn=repl-<db>,cn=slaptain-auth" read by dn.exact="cn=replication,<suffix>" read by * none` (two `by` clauses since the ADR-027 cutover, 2026-09-15 amendment — one rule, still ending `by * none`; the `olcLimits` exemption doubles with it, because a limits value carries exactly one selector); without it a data DB's ACLs are bypassable through its own change journal — *Accepted (impl + e2e green 2026-08-25; the bypass was captured live before the fix — an anonymous read of the shared journal returned `reqMod: userPassword:+ {SSHA}…` for a user whose `userPassword` the data DB denies)*

---

### slctl — Diagnostic CLI

`bin/slctl` (also installed to `/usr/local/bin/slctl`) is a diagnostic and management tool
for SlapdCluster resources. Source: `operator/cmd/slctl/`. Use it when debugging e2e failures
or inspecting cluster state.

| Command | Purpose |
|---|---|
| `slctl status [-n ns] [name]` | Quick overview: phase, replicas, replication, conditions |
| `slctl inspect [-n ns] [name]` | Per-pod LDAP queries + automated consistency checks (CSN convergence, topology, stanza counts). Per-database quantities are gathered and judged **per data suffix** — `contextCSN` is probed for every non-`cn=` naming context and the `csn-convergence` verdict is the worst over databases, naming the one it indicts (ADR-008 amendment 2026-09-14), alongside the ADR-025 suffix probes. `--short` for CI (checks only — every database's verdict still shows). `--json` carries `pods[].contextCSNBySuffix`, `pods[].syncReplBySuffix` and `pods[].multiProviderBySuffix`. Since 2026-09-28 the same per-suffix rule covers the syncrepl side: `syncrepl-stanza-count`, `syncrepl-skip-self`, `multi-provider`, `external-syncrepl` and `rid-uniqueness` judge **every replicated database** (scoped by the CRs' `replication.enabled`, so a non-replicated database is not held to a stanza count), and the per-pod display labels each stanza set with its suffix. `rid-uniqueness` is now cross-database, which is the collision it exists for (ADR-031 per-DB `ridSegment`). `accesslog-mincsn` (2026-10-11) is the ITS#9398 storm detector: on every RW pod's accesslog DBs, read as the config admin, every `minCSN` sid must appear in `contextCSN` — a missing one means that pod refuses every consumer of the log with `sync cookie is stale`, while `csn-convergence` stays green (docs/ITS-9398-STORM-SIGNATURE.md); `tests/e2e.sh` runs it on every site after the suite. Exits non-zero on check failure |
| `slctl rid decode <rid>` | Read a syncrepl RID back as the coordinate ADR-031 made it: `rid = 1 + segment·M + offset`, `M = podsPerSite + maxSites`. **Works with no cluster** — the default layout (`8x12`) is compiled in, so the offline path is the base case and not a degraded one; `--offline` pins it, `--layout <P>x<S>` decodes against another mesh's shape. **Does better with one**: the segment resolves to the SlapdDatabase declaring that `ridSegment`, the peer site index to the site declaring that `serverIDIndex` (ADR-028), and the layout comes from the `SlapdMesh` actually in force. `--cluster` only needed when the namespace holds more than one. **It has no failure channel — it has five answers**, each a different next step for the reader (ADR-031 Consequences): a coordinate; `reserved` (rid 0, never allocated — look for an unassigned stanza); `outside-matrix` (inside slapd's 0..999 band, above this layout's `C·M` — most likely a stanza written before the ADR-031 renumber, and the one case this command exists for); `outside-band` (not a replica ID at all); `layout-unusable` (the fault is the layout, not the number). Anything unresolved is said in words — "no SlapdDatabase of cluster … declares ridSegment 15" — never left as silence, which would read identically to "resolved to nothing"; a layout that could not be read is decoded against the defaults and **labelled untrusted** rather than silently assumed. A local offset names the pod at that ordinal **of the site the stanza came from**, which is not necessarily the cluster you pointed at. `--json` carries the layout's derived `segmentWidth`/`maxSegments` so a consumer need not re-implement the formula |
| `slctl debug-dump [-n ns] <name>` | Collect CR YAML, pod logs, LDAP state (rootDSE, contextCSN, syncrepl, ACLs), services, PVCs, events, operator logs into a timestamped directory |
| `slctl debug [cluster] [--pod <ord>] [-- cmd...]` | Attach an ephemeral **slapd-toolkit** debug container to a slapd pod (`kubectl debug --target=slapd`, defaults to `bash`; slapd is PID 1 there, its LDAP on `localhost:1024`). **Auto-detects the namespace's PodSecurity level** via a server-side dry-run probe and adapts: **privileged** → root + `SYS_PTRACE`/`SYS_ADMIN`/`NET_ADMIN`/`NET_RAW`, slapd's *live* FS at `/proc/1/root/`, `strace -p 1` works; **baseline** → root, no added caps, slapd PVCs mounted **read-only** at `/config`/`/data`/`/accesslog` (warns, prints the label command to unlock full power); **restricted** → UID 1024 + the same read-only mounts. `--no-privileged` forces the mount mode. Toolkit image auto-derived from the target pod's *running* slapd image (not the CR spec, so it works when `spec.images` is operator-defaulted; `--image` to override). The slapd container's own hardened security context is never touched. NB: `kubectl debug` ephemeral containers persist on the pod until it is recreated |
| `slctl aws --from <kind>/<name> [aws-args...]` | Wraps the system `aws` CLI the way `ldapsearch` is wrapped: the bucket's endpoint and credentials come from a slaptain resource, everything else passes through verbatim. **The source is always named** — `sb`/`ssb`/`sd`/`sr` (`spec.storage`, `spec.storage`, `spec.bootstrapFrom.s3.storage`, `spec.source.s3.storage`). Four kinds carry an `S3StorageSpec` and they routinely disagree, so there is no well-defined "the obvious one"; slctl discovers connection details, it does not guess intent — the deliberate asymmetry with `ldapsearch`, where a namespace's one SlapdCluster IS a fact to discover. Credentials (`access-key-id`/`secret-access-key`) go into the child's ENVIRONMENT, never argv, and an inherited `AWS_ENDPOINT_URL`/`AWS_SESSION_TOKEN`/`AWS_REGION` never outranks the resolved one. **Endpoint:** a cluster-internal Service DNS name (`<svc>.<ns>.svc[.<domain>]`, or a single label) is port-forwarded and rewritten to `localhost` — that is the whole point, since the endpoint is written for the pods, not for your laptop; anything else is used as-is and an empty one means real AWS. `<svc>.<ns>` is NOT treated as in-cluster (indistinguishable from `s3.example`). A scheme is required: guessing http vs https wrong sends a credential in clear. `insecureTLS` prepends `--no-verify-ssl`; an https endpoint reached THROUGH a forward warns rather than silently disabling verification. `--verbose` prints the port-forward and the full invocation with the secret access key masked (`--show-password` to show it) |
| `slctl ldapsearch [slctl-flags] [ldapsearch-args...]` | Wraps system `ldapsearch` with auto-discovered `-H`/`-D`/`-w`. Endpoint preference: LoadBalancer → NodePort → direct pod IP → port-forward. `--as admin\|config\|replication\|<DN>` selects the bind identity (default `admin`); `--anonymous` skips the bind. `--pod <ord>` targets one pod (RW or RO) — direct pod IP when reachable, else a port-forward. `--ldaps` for TLS. `--cluster`/`--database` only needed when the namespace has more than one. **Direct pod IPs (pod-routed labs / on-node shells):** whether the client can reach pod IPs directly is a property of the network path, so it is **autodetected, not configured**: a specific (non-default) kernel FIB route covering the pod IP — the artifact left by whoever set up native routing — gates one short confirming TCP dial; machines with only a default route pay nothing and port-forward as before. `--direct` forces it (skips the route check), `SLCTL_DIRECT_POD_IPS=always\|never\|auto` overrides detection (e.g. `never` in odd overlapping-CIDR setups, `always` behind a router that holds the pod-CIDR route). **Endpoint overrides (dual-homed/lab clusters where the auto-picked NodePort node IP is unreachable):** `--port-forward` forces a port-forward to pod-0 even when a NodePort/LB Service exists; `--node-ip <ip>` overrides the node address used for a NodePort endpoint (mirrors e2e `E2E_NODE_ACCESS_IP`). **Transparency:** `--verbose` prints the reproducible `kubectl port-forward` + `ldap*` command line so you can run it by hand ("disenchant the magic"), with the bind password **masked** as `<password>`. `--show-password` prints it in clear — named so that landing a credential on a terminal and in shell history is never a surprise. `--redact-password` (the old opt-in, from when clear text was the default) is still accepted, is now redundant, and still wins if combined with `--show-password`: contradictory flags resolve toward the answer that cannot leak. Reversed 2026-09-28 — the original "labs, not our job to patronize" call holds right up until the terminal is on a projector, and "remember the extra flag" is exactly what nobody remembers while presenting. **Reading an accesslog:** a log DB (`cn=accesslog-<database>`) grants read to `cn=replication,<suffix>` and nothing else, with `cn=admin,cn=config` as its rootDN (ADR-020), so `--as config` and `--as replication` can read it while the default `--as admin` — the *data* rootDN — is **denied**; that is a different database with a different rootDN, not a bug. **A cluster with no database is still readable:** the SlapdDatabase lookup is conditional on the bind, not unconditional — only the identities a database *defines* (`admin`'s rootDN and credentials Secret, `replication`, the short-form literal DNs completed with its suffix) need one. `--as config` and `--anonymous` do not, so a freshly applied SlapdCluster — or one whose SlapdDatabase CRs have not landed yet — answers `cn=config` queries, which is exactly the window you want them in. Sibling commands: `ldapadd`, `ldapmodify`, `ldapdelete` (same flags, read LDIF from stdin or `-f`) |

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

That rejection is about the *operator*. A sidecar is a different case and ADR-033 Surface 3
adopts it: a container inside the pod reaches the socket through the shared `/run/openldap`
emptyDir and binds SASL EXTERNAL with no credential at all, and `cn=Monitor`'s ACL grants
read to any peercred identity (`dn.children="cn=peercred,cn=external,cn=auth"`).

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
     `LDAP_CLUSTER_HEADLESS_SVC` (`<name>-headless`), `DATABASE_DIRS_PATH`, and `LDAP_REPLICATION_PASSWORD`
     (from `<dbname>-credentials` / `replication-password`).

#### slapd-init Changes for Phase 2

Post-ADR-004, the init container handles **cn=config infrastructure only**. Anything that
lives in the data tree or references a specific data database is the SlapdDatabase
controller's responsibility.

Init container, gated on `LDAP_REPLICATION_ENABLED=true`:
- Load `accesslog` and `syncprov` modules.
- Create the per-database accesslog backing directories `/accesslog/<dbname>` from the mounted
  directory-list ConfigMap (`back-mdb` does not create `olcDbDirectory`). The log **databases** themselves, their syncprov
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

RID scheme (ADR-031, lives on SlapdDatabase.spec.replication.ridSegment): a RID is a
coordinate, `rid = 1 + segment·(P+S) + offset`, where the segment names the database and
the offset names the provider — a pod ordinal at the local site, or a peer site's declared
index. The segment is DECLARED and unique per cluster; the operator refuses a collision
rather than assigning one, and the charts refuse to render one.

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
