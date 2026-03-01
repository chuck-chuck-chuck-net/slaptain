# LLM_STARTUP.md

## Project: slaptain (Kubernetes OpenLDAP Operator)

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
- [ ] Operator Phase 3: cross-cluster replication via `ExternalPeers`, mTLS peer auth.

---

### Repository Layout

```
.
├── Makefile                        # Root build targets (see Makefile Targets below)
├── LLM_STARTUP.md
├── docs/
│   ├── BOOTSTRAP.md                # Cluster bootstrap internals (init container + operator phases)
│   ├── ONBOARDING.md               # Team onboarding: LDAP concepts, operator model, credential model
│   └── adrs/
│       ├── adr-001-double-reconcile-runs.md
│       └── adr-002-cn-config-node-local-operator-managed.md
├── charts/
│   ├── operator/                   # Helm chart for deploying the operator itself
│   │   ├── crds/                   # CRD YAML (synced from operator/config/crd/bases/ via make operator-manifests)
│   │   └── templates/              # deployment, RBAC, serviceaccount, metrics, networkpolicy
│   ├── slapd/                      # Standalone Helm chart (baseline / comparison / testing vehicle)
│   ├── slapd-cluster/              # Helm chart deploying a SlapdCluster CR (operator required)
│   └── slapd-test/                 # Test chart: bootstrap job + toolkit pod
├── images/
│   ├── slapd/Containerfile         # slapd runtime image
│   ├── slapd-init/Containerfile    # Bootstrap init container image
│   ├── slapd-toolkit/Containerfile # Toolkit image (ldap-utils, python3, pyyaml, ldap3)
│   └── operator/Containerfile      # Operator image (multi-stage, distroless/static)
├── operator/                       # kubebuilder v4 Go operator (own Go module)
│   ├── api/v1alpha1/
│   │   ├── slapdcluster_types.go   # Full CRD type definitions (all phases)
│   │   └── zz_generated.deepcopy.go
│   ├── internal/controller/
│   │   └── slapdcluster_controller.go
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
    ├── values.slapd.yaml           # Non-secret values for slapd / slapd-cluster charts
    ├── values.slapd-test.yaml      # Non-secret deployment-specific overrides for slapd-test
    ├── values.slapd-test.secret.yaml.sample   # Template: passwords + readpw hashes/plaintexts
    ├── README.md                   # Test suite documentation (quick-start cycle at top)
    ├── SOPS.md                     # SOPS/age secret management guide
    └── e2e/                        # Ginkgo e2e tests (go-ldap, client-go)
        ├── suite_test.go           # BeforeSuite: port-forward, rootDSE baseDN discovery, admin connect
        ├── helpers_test.go         # k8s/LDAP helpers (ldapSearch, ldapAdd, portForward, …)
        ├── slapd_test.go           # StatefulSet, Service, PVC, passwords Secret checks
        ├── bootstrap_test.go       # bootstrap Job and toolkit Deployment checks
        ├── ldap_test.go            # Directory content: base structure, user/group CRUD, ACL basics
        └── readpw_test.go          # cn=config access; readpw user bind + ACL enforcement
```

---

### Image Details

- **Build tooling:** Plain Containerfile + podman (apko dropped — only beneficial in the Wolfi ecosystem).
- **slapd runtime** (`images/slapd/`): `gcr.io/distroless/base-debian13` — glibc, libssl, ca-certs, no shell.
- **slapd-init** (`images/slapd-init/`): `debian:trixie-slim` — ephemeral bootstrap; needs shell + python3 + OpenLDAP tools.
- **operator** (`images/operator/`): `gcr.io/distroless/static-debian13:nonroot`, statically-linked Go binary, UID 65532. Builder stage uses `golang:1.25`.
- **User (slapd):** `openldap` (UID/GID 1024). Debian's slapd package creates this user; we `groupmod`/`usermod` to 1024.
- **Ports:** 1024 (ldap), 1025 (ldaps) — non-privileged. Service maps 389→1024 and 636→1025.
- **Mount Points:** `/ldap-config` (slapd.d config dir, PVC), `/ldap-data` (LMDB data, PVC), `/run/openldap` (socket, emptyDir), `/etc/openldap/tls` (TLS secret, optional).
- **Module path:** `/usr/lib/ldap` (Debian path). Modules loaded dynamically; plan to compile in statically later.
- **Schema path:** `/etc/ldap/schema/` (Debian path).
- **Init Container:** Generates `slapd.conf`, runs `slaptest` to produce `slapd.d` format, populates initial LDIFs.

---

### Operator Details

**Framework:** Go + kubebuilder v4 (`go/v4` plugin), controller-runtime v0.23.1, k8s API v0.35.0.

**API:**
- Group: `ldap.chuck-chuck-chuck.net`
- Kind: `SlapdCluster` (shortName: `sc`)
- Version: `v1alpha1`
- Scope: Namespaced

**CRD spec fields** (all phases baked in from day 1 — no breaking changes needed later):

| Field | Type | Notes |
|---|---|---|
| `spec.images.{slapd,init}.{repository,tag,pullPolicy}` | `SlapdImages` | Image config for both containers |
| `spec.ldap.domain` | string | LDAP domain in DC notation, e.g. `dc=example,dc=org` |
| `spec.ldap.credentialsSecretName` | string | Optional: reference an existing plaintext credentials Secret (`admin-password` + `root-password` keys); suppresses auto-generation of `<name>-credentials` |
| `spec.ldap.acls` | `[]string` | Ordered list of slapd.conf `access to ...` rules applied to every pod's `cn=config` by the operator; empty = preserve init-container defaults |
| `spec.ldap.forceRebootstrap` | bool | Force init container to re-bootstrap (destructive) |
| `spec.ldap.tls.{enabled,secretName}` | `SlapdTLSConfig` | TLS Secret must contain `tls.crt`, `tls.key`, `ca.crt` |
| `spec.replicas` | int32 | Default 1; replication is only active when `replicas > 1` AND `replication.enabled=true` |
| `spec.logLevel` | int32 | slapd `-d` flag, default 0 |
| `spec.persistence.{enabled,config,data}` | `SlapdPersistenceConfig` | PVC sizes and storage class; emptyDir when disabled |
| `spec.service.{type,ldapPort,ldapsPort}` | `SlapdServiceConfig` | ClusterIP service config, defaults 389/636 |
| `spec.resources` | `corev1.ResourceRequirements` | Container resource requests/limits |
| `spec.securityContext` | `*corev1.PodSecurityContext` | Defaults to runAsUser/runAsGroup/fsGroup=1024 |
| `spec.replication.{enabled,role,mode,peers,externalPeers,accessLogEnabled}` | `SlapdReplicationConfig` | N-way multi-master delta-syncrepl; active when `enabled=true` and `replicas > 1` |

**Status fields:** `phase` (Bootstrapping/Running/Degraded/Error), `readyReplicas`, `replicas`, `observedGeneration`, `bootstrapComplete`, `conditions`.

**Reconcile order:**
1. Fetch `SlapdCluster` — NotFound → return nil (deleted)
2. `reconcileSecret` — create `<name>-passwords` (plaintext `admin-password` + `replication-password`, create-only) + `<name>-config-password` (plaintext `root-password`, create-only); or read from `spec.ldap.credentialsSecretName`
3. `reconcileHeadlessService` — `<name>-headless`, `clusterIP: None` (SSA patch)
4. `reconcileClusterIPService` — `<name>` (bare name), ClusterIP (SSA patch)
5. `reconcileStatefulSet` — `serviceName: <name>-headless`; uses `volumeClaimTemplates` when persistence enabled (SSA patch)
6. `reconcileBootstrap` — connect to pod-0 via pod IP on port 1024, bind as data rootdn, add root + admin + (optionally) replication entries; sets `status.bootstrapComplete=true`; no-op when already complete (see `docs/BOOTSTRAP.md`)
7. `reconcileACLs` — for each pod ordinal 0..replicas-1: dial `<name>-<N>.<name>-headless.<ns>.svc.cluster.local:1024`, bind as `cn=admin,cn=config` (from `<name>-config-password`), compare current `olcAccess` values against `spec.ldap.acls` (stripping `{N}` prefixes), replace if different; logs warning and skips pods not yet reachable (retries on next reconcile)
8. Observe StatefulSet → update `status.phase`, `readyReplicas`, `bootstrapComplete`, conditions (SSA patch on status subresource)
9. Not Running → `RequeueAfter: 10s`

**Service naming (Bitnami convention):**
- Headless: `<name>-headless` — used by StatefulSet for pod DNS (`<name>-0.<name>-headless.ns.svc`)
- ClusterIP: `<name>` — client-facing, maps standard ports 389→1024 and 636→1025
- This ensures `SLAPD_HOST=slapd` works identically for standalone chart and operator deployments
- `charts/slapd-cluster` has `nameOverride: slapd` so `helm install slapd ./charts/slapd-cluster` → fullname `slapd`

**Owned resources:** StatefulSet, Service (×2), Secret, PersistentVolumeClaim — all get `SetControllerReference`.

**Known gotcha:** `kubebuilder init` requires `--skip-go-version-check` on Go 1.26 (version string not recognized). `kubebuilder create api` does not accept this flag and works without it.

---

### slapd-test Chart (`charts/slapd-test`)

Test harness chart with two components: a bootstrap `Job` and an optional toolkit `Deployment`.
Configuration lives entirely in values — no files are embedded in the chart image.

Key values structure:

| Value | Purpose |
|---|---|
| `slapd.domain` | LDAP base DN (e.g. `dc=chuck-chuck-chuck,dc=net`) |
| `slapd.adminPassword` / `rootPassword` | Plaintext passwords → stored in `slapd-test-passwords` Secret |
| `bootstrap.readpwOU` | OU name for read-only service accounts (default `Readpw`) |
| `bootstrap.customSchemaJson` | Optional JSON schema entry for `cn=config`; empty = no custom schema |
| `bootstrap.ousJson` | JSON array of OUs to create; supports Helm `tpl` expressions (domain/readpwOU substituted at render) |
| `bootstrap.readpwUsers` | `username → {SSHA}hash` — stored in LDAP entries |
| `bootstrap.readpwPasswords` | `username → plaintext` — stored in `slapd-test-passwords` Secret as `readpw-<user>` keys; used by e2e tests to bind as readpw users |

The `ousJson` / `customSchemaJson` values may contain `{{ }}` Helm template expressions.
Values files are plain YAML (no rendering); expressions are only evaluated when the configmap
template calls `tpl .Values.bootstrap.ousJson .`. This means `-f override.yaml` files can
contain template expressions and they work identically to chart default values.

**Note:** ACL management was removed from the slapd-test chart. ACLs are now declared in
`spec.ldap.acls` on the `SlapdCluster` CR and applied to every pod by the operator.
See ADR-002.

Deployment-specific overrides (e.g. OX schema, extra OUs) go in `tests/values.slapd-test.yaml`.
Secrets (passwords, SSHA hashes) go in `tests/values.slapd-test.secret.yaml` (SOPS-encrypted).
See `tests/values.slapd-test.secret.yaml.sample` for the expected structure.

### e2e Test Suite (`tests/e2e/`)

**Typical cycle** (operator path; use `helm-install`/`helm-uninstall` for standalone):
```bash
make cluster-helm-install testing-helm-install
make e2e-run
make testing-helm-uninstall cluster-helm-uninstall
```

**Suite setup** (`suite_test.go` `BeforeSuite`):
1. Build k8s client
2. Wait for `slapd` StatefulSet ready
3. Wait for `slapd-test` bootstrap Job succeeded
4. Read `adminPW` from `slapd-passwords` Secret (`admin-password` key)
5. Read `rootPW` from `slapd-config-password` Secret (`root-password` key)
6. Read `readpwPWs` from `slapd-test-passwords` Secret — `map[string]string` built from all `readpw-*` keys
7. Start `kubectl port-forward svc/slapd 13891:389` (local mode only; skipped when `LDAP_ADDR` is set)
8. Query LDAP rootDSE (anonymous, `namingContexts`) → set `baseDN` (auto-discovered, no env var)
9. Connect `ldapConn` as `cn=admin,<baseDN>` (data rootDN, bypasses ACLs)

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
| `make operator-helm-install` | `helm upgrade --install slaptain-operator ./charts/operator` |
| `make operator-helm-uninstall` | Uninstall the operator Helm release |
| `make gencert` | Generate self-signed TLS cert via `tests/gencert.sh` |
| `make helm-install` | Bare `helm upgrade --install slapd ./charts/slapd` |
| `make helm-deploy` | Full pipeline: `push` + `gencert` + `helm-install` |
| `make helm-uninstall` | Uninstall the slapd Helm release |
| `make cluster-helm-install` | `helm upgrade --install slapd ./charts/slapd-cluster` |
| `make cluster-helm-uninstall` | Uninstall the slapd-cluster Helm release |
| `make testing-helm-install` | `helm upgrade --install slapd-test ./charts/slapd-test` |
| `make testing-helm-uninstall` | Uninstall the slapd-test Helm release |
| `make e2e-run` | Run Ginkgo e2e tests in `tests/e2e/` |

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
| slapd-init (init container) | Data admin + config admin passwords | Plaintext (hashed at runtime by `slappasswd`) | 1+ |
| Operator: bootstrap | Data admin password | Plaintext | 2 (implemented) |
| Operator: ACL management (`reconcileACLs`) | Config admin password | Plaintext | 2 (implemented) |
| Operator: topology reconfiguration | Config admin password | Plaintext | 3 |
| Operator: CSN lag monitoring | Read-only access to `contextCSN` / `cn=monitor` | Plaintext (monitoring DN) or anonymous | 3 |
| Consumer init: syncrepl bind | Replication bind password | Plaintext | 2 (implemented) |
| Bootstrap job (slapd-test) | Data admin + (optionally) config admin passwords | Plaintext | Testing only |

**Secrets layout:**

| Secret | Contents | Created by | Scope |
|---|---|---|---|
| `<name>-passwords` | `admin-password` + `replication-password` (plaintext) | Operator (auto-generated) or user (`spec.ldap.credentialsSecretName`) | Operator SA + init container; LDAP bind during bootstrap and syncrepl |
| `<name>-config-password` | `root-password` (plaintext) | Operator (auto-generated) or user (`spec.ldap.credentialsSecretName`) | Operator SA: ACL reconciliation (`reconcileACLs`) and Phase 3 topology management |
| `slapd-test-passwords` | `readpw-<user>` (plaintext, one key per readpw account) | slapd-test chart | Testing only; e2e tests bind as readpw users to verify ACLs |

**Production scope:** The operator provides a correctly configured, healthy LDAP endpoint.
Directory content (schemas, OUs, users) is the user's responsibility. The slapd-test bootstrap
job is a testing tool, not part of production deployment.

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
| Data DB (`olcDatabase={1}mdb`) | `/ldap-data` (existing) | LDAP data; unchanged from Phase 1 |
| Accesslog DB (`olcDatabase={2}mdb`) | `/ldap-accesslog` (**new PVC**) | Delta-syncrepl change journal |
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
| `ldap-config` | `ldap-config-<name>-<N>` | `/ldap-config` |
| `ldap-data` | `ldap-data-<name>-<N>` | `/ldap-data` |
| `ldap-accesslog` | `ldap-accesslog-<name>-<N>` | `/ldap-accesslog` (**new**) |

The `reconcilePVCs` controller step is **removed**; PVC lifecycle is managed entirely by the
StatefulSet. Breaking change for Phase 1 deployments (PVC names change). Acceptable at v1alpha1.

#### Replication Credentials

The replication bind password is stored in the `<name>-passwords` Secret alongside the admin
password (not in a separate Secret):

| Secret | Key | Used by |
|---|---|---|
| `<name>-passwords` | `replication-password` | Init container: syncrepl bind password; operator: adds `cn=replication` LDAP entry |

The operator generates a random `replication-password` when creating `<name>-passwords`. Users
may pre-populate it via `spec.ldap.credentialsSecretName`. Never updated after creation.

Replication bind DN: `cn=replication,<domain>`. The operator adds this entry via live LDAP
during bootstrap and the init container grants it read access to the accesslog and data databases.

#### Operator Changes for Phase 2

1. **Remove `replicas > 1` guard** (controller step 2 in Phase 1).
2. **Remove `reconcilePVCs`**: replaced by StatefulSet `volumeClaimTemplates`.
3. **Update `buildStatefulSetSpec`**:
   - Add `ldap-accesslog` volume mount to init and main containers.
   - Move config/data/accesslog to `volumeClaimTemplates`.
   - Pass to init container: `LDAP_REPLICATION_ENABLED`, `LDAP_REPLICAS`,
     `LDAP_CLUSTER_HEADLESS_SVC` (`<name>-headless`), and `LDAP_REPLICATION_PASSWORD`
     (from `<name>-passwords` / `replication-password`).

#### slapd-init Changes for Phase 2

The init script gains a conditional replication path, gated on `LDAP_REPLICATION_ENABLED=true`:

- Extend `slapd.conf` with: accesslog DB config, `overlay accesslog`, `overlay syncprov` on
  both databases, `mirrormode on`, and N-1 `syncrepl` blocks (one per peer ordinal, skipping self).
- Peer URL template: `ldaps://<name>-<ordinal>.<headless-svc>.<ns>.svc.cluster.local:1025`
- On first bootstrap: add `cn=replication` entry to config DB and its ACL grants.
- On pod restart (config exists): patch existing `cn=config` via `ldapmodify` on the ldapi
  socket to add/update syncrepl entries. This is what enables future scale-out without full
  re-bootstrap.

Pod ordinal is read from the hostname: `${HOSTNAME##*-}` (last segment of StatefulSet pod name).

#### Phase 2 Scope and Non-Goals

| In scope | Out of scope (Phase 3+) |
|---|---|
| Replicas > 1 with N-way multi-master | Changing replicas after cluster creation (scale-out) |
| Graceful pod failure and restart | Cross-cluster replication (ExternalPeers) |
| Replication credentials Secret | CSN lag monitoring and alerting |
| Per-pod accesslog PVC | Per-peer TLS client certificate auth |
| Operator-managed cn=config ACLs (`spec.ldap.acls`) | Operator-managed custom schemas (`spec.ldap.schemas`) |

---

### cn=config Architecture Note

`cn=config` is **node-local** — it is never replicated between pods. Each pod has its own
independent copy stored in the `/ldap-config` PVC. This affects anything that lives in
`cn=config`: ACLs (`olcAccess`), schemas, overlays, and syncrepl stanzas.

The operator manages cn=config state by connecting to each pod individually via the headless
service DNS (`<name>-<N>.<name>-headless.<ns>.svc.cluster.local`). The `reconcileACLs` step
(reconcile step 7) applies `spec.ldap.acls` to every pod on every reconcile loop and is
idempotent (compares current `olcAccess` values before issuing a modify).

Anything that touches `cn=config` outside the operator (e.g. the slapd-test bootstrap job's
`customSchemaJson`) only reaches one pod via ClusterIP and is not self-healing. See ADR-002.

---

### Backlog

Items that follow the same pattern as existing work but are deferred to a future phase.

#### Schema extensions (`spec.ldap.schemas`) — Phase 3

Custom LDAP schemas (e.g. the OX schema) currently live in the slapd-test chart's
`customSchemaJson` and are applied by the bootstrap job to one pod via ClusterIP. This has the
same cn=config node-locality problem as ACLs had before ADR-002.

The correct fix is a `spec.ldap.schemas` field on `SlapdCluster`, managed by the operator
using the same per-pod headless-DNS approach as `reconcileACLs`. The operator would compare the
desired schema OID/attributes with what each pod has in `cn=schema,cn=config` and apply
missing schemas.

Until this is implemented, schemas must either be baked into the init container's slapd.conf
(suitable for stable, well-known schemas) or accepted as "apply to one pod only" for
development environments.
