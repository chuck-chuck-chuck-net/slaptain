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
- [x] Helm Chart for standalone deployment (`charts/slapd`) — superseded by operator, kept for reference.
- [x] **Kubernetes Operator — Phase 1** (`operator/`): standalone single-replica StatefulSet managed by a kubebuilder controller. e2e: 33/33 green.
- [x] **Operator Phase 2**: N-way multi-master delta-syncrepl; operator-orchestrated bootstrap; per-pod `volumeClaimTemplates`; replication credential management. e2e: pending.
- [x] **Operator Phase 3**: cross-cluster replication via `ExternalPeers`, mTLS peer auth. Operator owns all syncrepl configuration (in-cluster + external). See ADR-003.
- [x] **S3 backup/restore** (ADR-014): `SlapdBackup` (on-demand) + `SlapdScheduledBackup` (cron + retention) → gzipped `slapcat` LDIF to S3 via co-located Jobs; `SlapdDatabase.spec.bootstrapFrom` restores into a fresh DB via a cluster-coordinated scale-to-0 → offline `slapadd` → scale-up machine. e2e green on t3e (versitygw S3 target). See `docs/BACKUP.md`.

---

### Repository Layout

```
.
├── Makefile                        # Root build targets (see Makefile Targets below)
├── CLAUDE.md
├── docs/
│   ├── BOOTSTRAP.md                # Cluster bootstrap internals (init container + operator phases)
│   ├── ONBOARDING.md               # Team onboarding: LDAP concepts, operator model, credential model
│   ├── MIGRATION-PLAN.md           # Phased plan for replacing a legacy OpenLDAP with slaptain
│   ├── MIGRATION-LEGACY-SOURCE.md  # Source-side (legacy slapd) prep for hot migration
│   ├── BACKUP.md                   # S3 backup/restore user guide (ADR-014)
│   ├── BACKUP-PLAN.md              # ADR-014 implementation breakdown (phases)
│   ├── BACKLOG.md                  # Cross-cutting tech debt (e.g. client.Apply deprecation)
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
│       └── adr-016-pod-routed-cross-cluster-replication.md
├── charts/
│   ├── operator/                   # Helm chart for deploying the operator itself
│   │   ├── crds/                   # CRD YAML (synced from operator/config/crd/bases/ via make operator-manifests)
│   │   └── templates/              # deployment, RBAC, serviceaccount, metrics, networkpolicy
│   ├── slapd/                      # Standalone Helm chart (baseline / comparison / testing vehicle)
│   ├── slapd-cluster/              # Helm chart deploying a SlapdCluster CR (operator required)
│   └── slapd-toolkit/              # Persistent debug pod (ldap-utils, python3, ldap3) wired to operator-managed Secrets
├── images/
│   ├── slapd/Containerfile         # slapd runtime image
│   ├── slapd-init/Containerfile    # Bootstrap init container image
│   ├── slapd-toolkit/Containerfile # Toolkit image (ldap-utils, python3, pyyaml, ldap3)
│   └── operator/Containerfile      # Operator image (multi-stage, distroless/static)
├── scripts/
│   └── create-remote-kubeconfig.sh # Cross-site RBAC + kubeconfig Secret provisioning (ADR-007)
├── operator/                       # kubebuilder v4 Go operator (own Go module)
│   ├── api/v1alpha1/
│   │   ├── slapdcluster_types.go   # SlapdCluster CRD (+ Restoring phase, status.restore — ADR-014)
│   │   ├── slapddatabase_types.go  # SlapdDatabase CRD (+ bootstrapFrom, restoreApplied)
│   │   ├── slapdschema_types.go    # SlapdSchema CRD
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
    ├── e2e.sh                      # Unified e2e orchestration (N=1 → single-site; N≥2 → multi-site)
    ├── e2e-singlesite.sh           # Backward-compat wrapper around e2e.sh
    ├── e2e-multisite.sh            # Backward-compat wrapper around e2e.sh
    ├── e2e-migration.sh            # Migration-scenario e2e (independent: slaptain + fake-prod topology)
    ├── resources/
    │   ├── example/                # Open-source test fixtures (SlapdDatabase, SlapdSchema, Secrets)
    │   ├── versitygw.yaml          # Lean S3 server (Apache-2.0) for backup e2e — NOT minio
    │   └── lab/                    # Internal lab configuration (SOPS-encrypted secrets)
    ├── README.md                   # Test suite documentation (quick-start cycle at top)
    ├── SOPS.md                     # SOPS/age secret management guide
    └── e2e/                        # Ginkgo e2e tests (go-ldap, client-go)
        ├── suite_test.go           # BeforeSuite: NodePort LDAP connect, rootDSE baseDN discovery, admin connect
        ├── helpers_test.go         # k8s/LDAP helpers (ldapSearch, ldapAdd, dialPodLDAP, …)
        ├── slapd_test.go           # StatefulSet, Service, PVC, passwords Secret checks
        ├── bootstrap_test.go       # SlapdDatabase Running checks
        ├── ldap_test.go            # Directory content: base structure, user/group CRUD, ACL basics
        ├── readpw_test.go          # cn=config access; readpw user bind + ACL enforcement
        ├── readonly_test.go        # Read-only replica tests: data sync, write rejection
        ├── resilience_test.go      # Pod-restart resilience (warm restart labelled persistent-only; gated E2E_RESILIENCE=1)
        ├── dataloss_recovery_test.go # Pod loses its PVCs (kubectl delete pod + pvc); replication restores DIT (ADR-012 case 2)
        ├── migration_test.go       # Migration scenario (gated at registration time: E2E_MIGRATION=1)
        ├── external_replication_test.go  # Cross-cluster replication (gated: E2E_EXTERNAL_REPL=1)
        ├── backup_test.go          # SlapdBackup → S3 round-trip (gated: E2E_BACKUP=1, deploys versitygw)
        └── restore_test.go         # bootstrapFrom restore into a fresh cluster (gated: E2E_BACKUP=1)
```

---

### Image Details

- **Build tooling:** Plain Containerfile + podman (apko dropped — only beneficial in the Wolfi ecosystem).
- **slapd runtime** (`images/slapd/`): `gcr.io/distroless/base-debian13` — glibc, libssl, ca-certs, no shell.
- **slapd-init** (`images/slapd-init/`): `debian:trixie-slim` — ephemeral bootstrap; needs shell + python3 + OpenLDAP tools.
- **operator** (`images/operator/`): `gcr.io/distroless/static-debian13:nonroot`, statically-linked Go binary, UID 65532. Builder stage uses `golang:1.25`.
- **User (slapd):** `openldap` (UID/GID 1024). Debian's slapd package creates this user; we `groupmod`/`usermod` to 1024.
- **Ports:** 1024 (ldap), 1025 (ldaps) — non-privileged. Service maps 389→1024 and 636→1025.
- **Mount Points:** `/config` (slapd.d config dir, PVC), `/data` (LMDB data, PVC), `/accesslog` (delta-syncrepl change journal, PVC), `/run/openldap` (socket, emptyDir), `/etc/openldap/tls` (TLS secret, optional).
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
  with keys `access-key-id`/`secret-access-key`.
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
| `spec.logLevel` | int32 | slapd `-d` flag, default 0 |
| `spec.persistence.{config,data,accesslog}` | `SlapdPersistenceConfig` | PVC sizes / storage class / access mode (per ADR-013, persistence is mandatory; the `enabled` field was removed) |
| `spec.service.{type,ldapPort,ldapsPort}` | `SlapdServiceConfig` | ClusterIP service config, defaults 389/636 |
| `spec.resources` | `corev1.ResourceRequirements` | Container resource requests/limits |
| `spec.securityContext` | `*corev1.PodSecurityContext` | Defaults to runAsUser/runAsGroup/fsGroup=1024 |
| `spec.replication.{enabled,externalPeers}` | `SlapdReplicationConfig` | N-way multi-master delta-syncrepl; active when `enabled=true` and `replicas > 1` |
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

Database-level config (ACLs, schemas, indices, replication per-DB) is declared on `SlapdDatabase` and `SlapdSchema` CRs.

**Status fields:** `phase` (Bootstrapping/Running/Degraded/Error/**Restoring**), `readyReplicas`, `replicas`, `readOnlyReadyReplicas`, `readOnlyReplicas`, `observedGeneration`, `replicationNetworkIPs` (discovered Multus IPs per pod), `externalPeerStatuses` (per-peer: `replicationState` Synced/Lagging/Unreachable, `lagSeconds`, `lastChecked`, `discoveredAddresses`), `restore` (in-progress bootstrapFrom restore: sub-`phase`, `originalReplicas`, `databases` — ADR-014), `conditions` (including `ReplicationConverged` for local CSN convergence).

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
- This ensures `SLAPD_HOST=slapd` works identically for standalone chart and operator deployments
- `charts/slapd-cluster` has `nameOverride: slapd` so `helm install slapd ./charts/slapd-cluster` → fullname `slapd`

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

**Node access (NodePort reachability):** the runner reaches slapd via NodePorts on
a per-context *node access IP*, defaulting to each node's k8s `InternalIP`. On
dual-homed clusters that default is wrong when the `InternalIP` is on a network the
runner can't reach north-south (e.g. a routed replication network chosen as the
primary node network) — and the reachable NIC isn't k8s-registered, so it can't be
auto-discovered. Override it: `E2E_NODE_ACCESS_IP=<ip>`
(single-site) or `E2E_NODE_ACCESS_IPS="ctx=ip ..."` (multi-site). This drives
`LDAP_ADDR`/`E2E_REMOTE_LDAP_ADDR`, the suite's `E2E_NODE_IP`, and the TLS cert
SAN; cross-site peer URIs keep the `InternalIP` (they must ride the replication
network). See `tests/README.md`.

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
| `make all` | Build all three images |
| `make build-init` | Build slapd-init image |
| `make build-slapd` | Build slapd image |
| `make build-operator` | Build operator image (build context = repo root) |
| `make push` | Build + push all three images |
| `make operator-generate` | Run `make generate` in `operator/` (regenerates deepcopy) |
| `make operator-manifests` | Run `make manifests` in `operator/`, then sync CRD to `charts/operator/crds/` |
| `make operator-sync-crd` | Copy CRD from `operator/config/crd/bases/` to `charts/operator/crds/` |
| `make operator-crd-apply` | `kubectl apply --server-side` the CRDs from `operator/config/crd/bases/` (Helm only installs `crds/` on first `install`, never on `upgrade`) |
| `make operator-helm-install` | `helm upgrade --install slaptain-operator ./charts/operator` (depends on `operator-crd-apply`, so a new/changed CRD lands on upgrade too). t3e loop: `make operator-helm-install CONTEXT=t3e GIT_TAG=<tag>` |
| `make operator-helm-uninstall` | Uninstall the operator Helm release |
| `make gencert` | Generate self-signed TLS cert via `tests/gencert.sh` |
| `make helm-install` | Bare `helm upgrade --install slapd ./charts/slapd` |
| `make helm-deploy` | Full pipeline: `push` + `gencert` + `helm-install` |
| `make helm-uninstall` | Uninstall the slapd Helm release |
| `make cluster-helm-install` | `helm upgrade --install slapd ./charts/slapd-cluster` |
| `make cluster-helm-uninstall` | Uninstall the slapd-cluster Helm release |
| `make testing-apply` | `kubectl apply` test resources (SlapdDatabase, SlapdSchema, Secrets) |
| `make testing-delete` | `kubectl delete` test resources |
| `make toolkit-install` | `helm upgrade --install toolkit ./charts/slapd-toolkit` (debug pod) |
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

# 3. Prereqs for a SlapdCluster: namespace + TLS secret
kubectl create namespace slaptain
make gencert   # creates slapd-tls secret in slaptain namespace

# 4. Apply sample CR
kubectl apply -f operator/config/samples/ldap_v1alpha1_slapdcluster.yaml

# 5. Verify
kubectl get sc -n slaptain
kubectl get statefulset,svc,secret,pvc -n slaptain -l app.kubernetes.io/instance=slapd
kubectl rollout status statefulset/slapd -n slaptain --timeout=120s

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
- ADR-002: cn=config is node-local; the operator manages it per-pod
- ADR-003: Operator owns all syncrepl configuration (RID scheme, single source of truth)
- ADR-004: Multi-resource CRD architecture (SlapdCluster / SlapdSchema / SlapdDatabase)
- ADR-005: SlapdDatabase cleanup policy (Retain default, Delete opt-in)
- ADR-006: Schema lifecycle (additive-only, desired-minimum model)
- ADR-007: Multus-based dedicated replication network for cross-site traffic (amended: dynamic peer discovery via remote kubeconfig)
- ADR-008: CSN monitoring uses replication bind credentials (uniform-password assumption)
- ADR-009: SlapdUser lifecycle (service users only, single-pod write, retain default) — *Proposed*
- ADR-010: SlapdCluster replication modes (peer / consumer-only, in-place promotion) — *Accepted (impl + e2e green 2026-05-13)*
- ADR-011: Hot migration topology contract (RID/ServerID coexistence, plain-syncrepl interop, stage transitions) — *Accepted (impl + e2e green 2026-05-13)*
- ADR-012: Seed is one-shot; cluster wipe is a Kubernetes resource lifecycle operation (replaces removed `forceRebootstrap` + reverted `verifySeedExists`)
- ADR-013: Defer hot SlapdDatabase add/remove; require persistent storage (rolling restart on DB add/remove accepted as UX wart on persistent storage)
- ADR-014: S3 backup/restore (slapcat→gzip→S3 via co-located Job; bootstrapFrom restore into a fresh DB) — *Accepted (impl + e2e green on t3e 2026-06-09)*
- ADR-015: Cluster DNS domain is resolved (CLUSTER_DOMAIN env → resolv.conf → cluster.local), never hardcoded — *Accepted 2026-07-05*
- ADR-016: Direct native pod-IP routing as a cross-cluster replication transport (alongside Multus/NodePort; `network.mode: pod-routed`) — *Accepted (impl + e2e green across three routed-pod-CIDR sites 2026-07-06)*

---

### slctl — Diagnostic CLI

`bin/slctl` (also installed to `/usr/local/bin/slctl`) is a diagnostic and management tool
for SlapdCluster resources. Source: `operator/cmd/slctl/`. Use it when debugging e2e failures
or inspecting cluster state.

| Command | Purpose |
|---|---|
| `slctl status [-n ns] [name]` | Quick overview: phase, replicas, replication, conditions |
| `slctl inspect [-n ns] [name]` | Per-pod LDAP queries + automated consistency checks (CSN convergence, topology, stanza counts). `--short` for CI. Exits non-zero on check failure |
| `slctl debug-dump [-n ns] <name>` | Collect CR YAML, pod logs, LDAP state (rootDSE, contextCSN, syncrepl, ACLs), services, PVCs, events, operator logs into a timestamped directory |
| `slctl debug [cluster] [--pod <ord>] [-- cmd...]` | Attach an ephemeral **slapd-toolkit** debug container to a slapd pod (`kubectl debug --target=slapd`, defaults to `bash`; slapd is PID 1 there, its LDAP on `localhost:1024`). **Auto-detects the namespace's PodSecurity level** via a server-side dry-run probe and adapts: **privileged** → root + `SYS_PTRACE`/`SYS_ADMIN`/`NET_ADMIN`/`NET_RAW`, slapd's *live* FS at `/proc/1/root/`, `strace -p 1` works; **baseline** → root, no added caps, slapd PVCs mounted **read-only** at `/config`/`/data`/`/accesslog` (warns, prints the label command to unlock full power); **restricted** → UID 1024 + the same read-only mounts. `--no-privileged` forces the mount mode. Toolkit image auto-derived from the cluster's slapd image (`--image` to override). The slapd container's own hardened security context is never touched. NB: `kubectl debug` ephemeral containers persist on the pod until it is recreated |
| `slctl ldapsearch [slctl-flags] [ldapsearch-args...]` | Wraps system `ldapsearch` with auto-discovered `-H`/`-D`/`-w`. Endpoint preference: LoadBalancer → NodePort → port-forward. `--as admin\|config\|replication\|<DN>` selects the bind identity (default `admin`); `--anonymous` skips the bind. `--pod <ord>` forces a port-forward to one pod (RW or RO). `--ldaps` for TLS. `--cluster`/`--database` only needed when the namespace has more than one. Sibling commands: `ldapadd`, `ldapmodify`, `ldapdelete` (same flags, read LDIF from stdin or `-f`) |

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
| Accesslog DB (`olcDatabase={2}mdb`) | `/accesslog` (**new PVC**) | Delta-syncrepl change journal |
| `overlay accesslog` on data DB | — | Writes every change to accesslog DB |
| `overlay syncprov` on accesslog DB | — | Exposes change journal to peers |
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
| `accesslog` | `accesslog-<name>-<N>` | `/accesslog` (**new**) |

The `reconcilePVCs` controller step is **removed**; PVC lifecycle is managed entirely by the
StatefulSet. Breaking change for Phase 1 deployments (PVC names change). Acceptable at v1alpha1.

#### Replication Credentials

The replication bind password is stored in the `<dbname>-credentials` Secret alongside the
data admin password (per-database):

| Secret | Key | Used by |
|---|---|---|
| `<dbname>-credentials` | `replication-password` | Init container: syncrepl bind password; SlapdDatabase controller: adds `cn=replication` LDAP entry |

The SlapdDatabase controller generates a random `replication-password` when creating
`<dbname>-credentials`. Never updated after creation.

Replication bind DN: `cn=replication,<suffix>`. The controller adds this entry via live LDAP
during bootstrap and the init container grants it read access to the accesslog and data databases.

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
- Create the accesslog DB (`olcDatabase={N}mdb cn=accesslog`) with its backing directory.
- Add `overlay syncprov` to the accesslog DB so peers can pull incremental updates.
- Accesslog DB ACLs granting `cn=replication,<suffix>` read access.
- Skip accesslog setup entirely when `LDAP_READONLY_REPLICA=true` (RO pods don't produce changes).

SlapdDatabase controller, per data database, when `spec.replication.deltaSync=true`:
- Add `overlay accesslog` and `overlay syncprov` to the data DB (`ensureReplicationOverlays`).
- Set `olcMultiProvider: TRUE` on the data DB (the OL 2.5+ rename of `olcMirrorMode`).
- Write N-1 in-cluster `olcSyncRepl` stanzas plus external peer stanzas (see ADR-003).
- Add `cn=replication,<suffix>` bind entry to the data tree (`ensureReplicationUser`).
- Prepend an ACL granting `cn=replication,<suffix>` read-all (see reconcile-loop-fixes.md:
  "User ACLs block userPassword replication").

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
