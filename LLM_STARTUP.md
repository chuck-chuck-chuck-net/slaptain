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
- [x] **Kubernetes Operator — Phase 1** (`operator/`): standalone single-replica StatefulSet managed by a kubebuilder controller.
- [ ] Operator Phase 2: multi-replica intra-cluster delta-syncrepl.
- [ ] Operator Phase 3: cross-cluster replication via `ExternalPeers`, mTLS peer auth.
- [ ] Replication logic in slapd-init (init container configures syncrepl per pod ordinal).

---

### Repository Layout

```
.
├── Makefile                        # Root build targets (see Makefile Targets below)
├── LLM_STARTUP.md
├── charts/
│   ├── operator/                   # Helm chart for deploying the operator itself
│   │   ├── crds/                   # CRD YAML (synced from operator/config/crd/bases/ via make operator-manifests)
│   │   └── templates/              # deployment, RBAC, serviceaccount, metrics, networkpolicy
│   ├── slapd/                      # Legacy standalone Helm chart (reference only)
│   └── slapd-test/                 # Test Helm chart
├── images/
│   ├── slapd/Containerfile         # slapd runtime image
│   ├── slapd-init/Containerfile    # Bootstrap init container image
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
    └── gencert.sh                  # TLS cert generation helper
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
| `spec.ldap.passwordSecretName` | string | Reference existing Secret; suppresses Secret creation |
| `spec.ldap.{adminPasswordHash,rootPasswordHash}` | string | SSHA/bcrypt hashes; used when `passwordSecretName` is unset |
| `spec.ldap.forceRebootstrap` | bool | Force init container to re-bootstrap (destructive) |
| `spec.ldap.tls.{enabled,secretName}` | `SlapdTLSConfig` | TLS Secret must contain `tls.crt`, `tls.key`, `ca.crt` |
| `spec.replicas` | int32 | Default 1; Phase 1 enforces ≤1 (controller sets `Error` if >1) |
| `spec.logLevel` | int32 | slapd `-d` flag, default 0 |
| `spec.persistence.{enabled,config,data}` | `SlapdPersistenceConfig` | PVC sizes and storage class; emptyDir when disabled |
| `spec.service.{type,ldapPort,ldapsPort}` | `SlapdServiceConfig` | ClusterIP service config, defaults 389/636 |
| `spec.resources` | `corev1.ResourceRequirements` | Container resource requests/limits |
| `spec.securityContext` | `*corev1.PodSecurityContext` | Defaults to runAsUser/runAsGroup/fsGroup=1024 |
| `spec.replication.{enabled,role,mode,peers,externalPeers,accessLogEnabled}` | `SlapdReplicationConfig` | Phase 2+ fields; stored but ignored in Phase 1 |

**Status fields:** `phase` (Bootstrapping/Running/Degraded/Error), `readyReplicas`, `replicas`, `observedGeneration`, `conditions`.

**Reconcile order (Phase 1):**
1. Fetch `SlapdCluster` — NotFound → return nil (deleted)
2. Phase 1 guard: `replicas > 1` → set `phase=Error`, condition `Ready=False/UnsupportedReplicas`, return (no requeue)
3. `reconcileSecret` — create `<name>-passwords` Secret (create-only, never update)
4. `reconcilePVCs` — create `<name>-config` and `<name>-data` PVCs (create-only, never update)
5. `reconcileHeadlessService` — `<name>`, `clusterIP: None` (createOrUpdate)
6. `reconcileClusterIPService` — `<name>-svc` (createOrUpdate)
7. `reconcileStatefulSet` — mirrors Helm chart exactly (createOrUpdate)
8. Observe StatefulSet → update `status.phase`, `readyReplicas`, conditions
9. Not Running → `RequeueAfter: 10s`

**Owned resources:** StatefulSet, Service (×2), Secret, PersistentVolumeClaim — all get `SetControllerReference`.

**Known gotcha:** `kubebuilder init` requires `--skip-go-version-check` on Go 1.26 (version string not recognized). `kubebuilder create api` does not accept this flag and works without it.

---

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
| `make helm-install` | Build, push, gencert, then helm upgrade/install |

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
