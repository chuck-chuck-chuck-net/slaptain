# Slaptain

[![GitHub](https://img.shields.io/badge/github-chuck--chuck--chuck--net%2Fslaptain-blue?logo=github)](https://github.com/chuck-chuck-chuck-net/slaptain)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

A Kubernetes operator for deploying OpenLDAP (slapd) as a highly available, replicated directory service.

Slaptain manages the full lifecycle of multi-master OpenLDAP clusters on Kubernetes: N-way delta-syncrepl replication, per-database ACLs and custom schemas, read-only consumer replicas, and cross-cluster peering. It provides the control plane that static Helm charts and bootstrap scripts cannot — self-healing configuration, declarative database management, and proper bootstrap sequencing.

## Quick Start

### 1. Install the Operator

```bash
helm upgrade --install slaptain oci://ghcr.io/chuck-chuck-chuck-net/charts/slaptain \
  -n slaptain-system --create-namespace
```

This installs the RBAC, the operator Deployment, and the CRDs: `SlapdCluster`,
`SlapdDatabase`, `SlapdSchema`, `SlapdMesh`, `SlapdBackup`, `SlapdScheduledBackup`
and `SlapdRestore`.

The other charts are published to the same registry, and each answers a
different question:

| Chart | Use it for |
|---|---|
| `slaptain` | The control plane. The other three need it. |
| `slapd-mesh` | A whole multi-site mesh — same values file, applied at every site. |
| `slapd` | One site's `SlapdCluster`. |
| `slapd-toolkit` | A debug pod wired to an operator-managed cluster. |

```bash
helm pull oci://ghcr.io/chuck-chuck-chuck-net/charts/<chart> --version <X.Y.Z>
```

### Upgrading an existing install? Apply the CRDs first

`helm upgrade` does **not** update CustomResourceDefinitions — they are cluster-scoped and
shared between releases, so Helm declines to own them, and `--skip-crds` only turns the
install off; no flag updates an existing one. Apply them yourself, then upgrade:

```bash
kubectl apply --server-side --force-conflicts -f \
  https://github.com/chuck-chuck-chuck-net/slaptain/releases/download/v0.6.0/slaptain-crds.yaml

helm upgrade slaptain oci://ghcr.io/chuck-chuck-chuck-net/charts/slaptain \
  -n slaptain-system --version 0.6.0
```

`--force-conflicts` is required, not defensive: Helm installed the CRDs as the field manager
`helm`, so a plain server-side apply refuses with `conflict with "helm": .spec.versions`.

**Skipping this does not fail — it fails silently.** The API server prunes fields the CRD does
not know about without an error. Against a 0.5.0 CRD that drops all of `spec.placement`, so no
spread constraint is generated and no pod moves — which is exactly what a correct upgrade also
looks like. Check the CRD, not the pods, and see
[Upgrading](docs/UPGRADING.md#apply-the-crds-before-upgrading-the-operator) for the probe.

`--version` takes the bare version, **without the leading `v`** — `--version 0.6.0` for the
`v0.6.0` release. Helm passes it through to the registry as a tag and does not normalise it,
so the wrong spelling is a 404 rather than a warning. See [Versioning](docs/VERSIONING.md).

### 2. Deploy a Cluster

Create a TLS Secret and a `SlapdCluster` CR (infrastructure only — no databases yet):

```bash
# Any TLS Secret with tls.crt/tls.key (and ca.crt for a private CA) will do —
# cert-manager, your own PKI, or, from a clone of this repo, the self-signed
# helper: make gencert creates the slaptain-testing namespace and the slapd-tls
# Secret in it.
make gencert

# Single replica (simplest)
kubectl apply -n slaptain-testing -f operator/config/samples/ldap_v1alpha1_slapdcluster.yaml
```

Or, entirely through Helm — the same `SlapdCluster`, written from `values.yaml`:

```bash
helm upgrade --install slapd oci://ghcr.io/chuck-chuck-chuck-net/charts/slapd \
  -n slaptain-testing --create-namespace -f my-values.yaml
```

For a replicated cluster, set `replicas` and `replication.enabled`:

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdCluster
metadata:
  name: slapd
spec:
  # Optional. Omit it and the operator derives both images from its own image
  # and tag, which keeps slapd, slapd-init and the operator on one version.
  # Set a repository to point at a mirror, or a tag to pin (e.g. the -ol26 pair).
  images:
    slapd:
      repository: registry.example.com/slaptain/slapd
    init:
      repository: registry.example.com/slaptain/slapd-init

  ldap:
    tls:
      enabled: true
      secretName: slapd-tls

  replicas: 3
  replication:
    enabled: true

  readReplicas: 1

  # Persistence is mandatory (ADR-013) — this sizes it, it does not enable it,
  # and it is optional: unset takes the operator's defaults (config 1Gi,
  # data 5Gi, accesslog 1Gi). Size data to your directory — see docs/TUNING.md.
  persistence:
    data:
      size: 50Gi
```

### 3. Add a Database

Create a `SlapdDatabase` CR to add a data database to the cluster:

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdDatabase
metadata:
  name: myapp
spec:
  clusterRef: slapd
  suffix: "dc=example,dc=org"

  acls:
    - 'to attrs=userPassword by self write by anonymous auth by * none'
    - 'to * by * read'

  indices:
    - "objectClass eq"
    - "uid eq,sub"
    - "entryCSN eq"
    - "entryUUID eq"

  replication:
    ridSegment: 1
    deltaSync: true

  # On a multi-site mesh, add `site: <founder>` here: exactly ONE site may apply
  # a seed. Several sites seeding one suffix independently produces a permanent
  # hidden glue entry that reads healthy on every CSN check (ADR-025).
  seed:
    entries:
      - |
        dn: dc=example,dc=org
        objectClass: top
        objectClass: dcObject
        objectClass: organization
        o: example
        dc: example
      - |
        dn: ou=People,dc=example,dc=org
        objectClass: organizationalUnit
        ou: People
```

### 4. Add Custom Schemas (optional)

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdSchema
metadata:
  name: myapp-schema
spec:
  clusterRef: slapd
  priority: 100
  attributeTypes:
    - >-
      ( 1.3.6.1.4.1.99999.1.1.1 NAME 'myAppId'
        EQUALITY integerMatch SYNTAX 1.3.6.1.4.1.1466.115.121.1.27 SINGLE-VALUE )
  objectClasses:
    - >-
      ( 1.3.6.1.4.1.99999.1.2.1 NAME 'myAppUser'
        SUP inetOrgPerson STRUCTURAL MAY myAppId )
```

### 5. Verify Health

```bash
kubectl get slapdcluster
# NAME    PHASE     READY   REPLICAS   AGE
# slapd   Running   3       3          2m

kubectl get slapddatabase
# NAME    CLUSTER   SUFFIX              PHASE     AGE
# myapp   slapd     dc=example,dc=org   Running   1m

kubectl get slapdschema
# NAME            CLUSTER   PRIORITY   APPLIED   AGE
# myapp-schema    slapd     100        true      1m
```

For anything past "is it Running", use **`slctl`** — the diagnostic CLI shipped
with the operator. `slctl inspect` queries every pod directly and runs
consistency checks (CSN convergence per database, topology, suffix visibility),
`slctl debug-dump` collects a full incident bundle, and `slctl ldapsearch`
wraps `ldapsearch` with the endpoint and credentials discovered for you. See
[`docs/slctl.md`](docs/slctl.md).

## Sample Manifests

One commented example per CRD lives in
**[`operator/config/samples/`](operator/config/samples/)** — the shape of every
field, with the non-obvious ones explained inline:

| File | Kind |
|---|---|
| [`ldap_v1alpha1_slapdmesh.yaml`](operator/config/samples/ldap_v1alpha1_slapdmesh.yaml) | `SlapdMesh` — the site inventory of a multi-site deployment |
| [`ldap_v1alpha1_slapdcluster.yaml`](operator/config/samples/ldap_v1alpha1_slapdcluster.yaml) | `SlapdCluster` — one site's server fleet |
| [`ldap_v1alpha1_slapddatabase.yaml`](operator/config/samples/ldap_v1alpha1_slapddatabase.yaml) | `SlapdDatabase` — suffix, credentials, ACLs, indices, seed |
| [`ldap_v1alpha1_slapdschema.yaml`](operator/config/samples/ldap_v1alpha1_slapdschema.yaml) | `SlapdSchema` — attributeTypes and objectClasses |
| [`ldap_v1alpha1_slapdbackup.yaml`](operator/config/samples/ldap_v1alpha1_slapdbackup.yaml) | `SlapdBackup` — one on-demand backup to S3 |
| [`ldap_v1alpha1_slapdscheduledbackup.yaml`](operator/config/samples/ldap_v1alpha1_slapdscheduledbackup.yaml) | `SlapdScheduledBackup` — cron schedule plus retention |
| [`ldap_v1alpha1_slapdrestore.yaml`](operator/config/samples/ldap_v1alpha1_slapdrestore.yaml) | `SlapdRestore` — in-place rollback of a populated database |

Apply one directly, or copy it and edit:

```bash
kubectl apply -n slaptain-testing -f operator/config/samples/ldap_v1alpha1_slapdcluster.yaml
```

Authoritative field documentation is the CRD itself — `kubectl explain
slapdcluster.spec.replication`, or the Go types in
[`operator/api/v1alpha1/`](operator/api/v1alpha1/).

## The data-plane images: which slapd you get, and why

Slaptain ships **four** slapd/slapd-init pairs. You pick one with two fields
that name what they govern, rather than by typing a tag:

```yaml
spec:
  images:
    select:
      storageFormat: lmdb1.0   # what can open your volumes — lmdb1.0 | lmdb0.9
      flavour: pure            # whether you run our patches — pure | slaptain
```

| `storageFormat` | `flavour` | what it is |
|---|---|---|
| `lmdb1.0` | `pure` | **the default** — OpenLDAP 2.7.2, no slaptain patches |
| `lmdb1.0` | `slaptain` | 2.7.2 plus our behaviour patches |
| `lmdb0.9` | `pure` | OpenLDAP 2.6, Debian's own build |
| `lmdb0.9` | `slaptain` | 2.6 plus our behaviour patches |

The selector drives **both** images from one declaration, which is why "pin
both or neither" is not a rule you can get wrong. An explicit
`spec.images.slapd.tag` still wins and is used verbatim — the selector picks a
build, it does not take a pin away from anyone who wants one.

**The two axes are named for what they decide, not for a version.** Keying on
the storage format means we can keep shipping newer slapd builds — security
fixes included — under the same selection, because the only thing that can
strand a volume is the axis you pinned yourself.

### Why 2.7 is the default

Not because of a bug fix. An earlier version of this page said 2.7 was adopted
to escape a `sync cookie is stale` refresh storm; that reasoning was
**retracted** — the storm is
[ITS#9398](https://bugs.openldap.org/show_bug.cgi?id=9398), it is fixed in no
release of either line, and slaptain addresses it with a log-database
checkpoint that works on both (see
[`docs/ITS-9398-HANDLING.md`](docs/ITS-9398-HANDLING.md)).

The real reason is simpler: **whoever takes the default never migrates.**
OpenLDAP 2.7 uses LMDB 1.0, 2.6 uses 0.9, and the two on-disk formats cannot
read each other. Defaulting to 2.6 would mean every cluster that said nothing
eventually pays a dump-and-reload to reach 2.7. Defaulting to 2.7 costs that
nobody. 2.6 stays available, deliberately chosen, for interoperating with an
existing 2.6 deployment during a migration (ADR-011).

### Why `pure` is the default flavour

Nobody should have to run a patched slapd in production without having chosen
to. The `slaptain` flavour carries our own behaviour patches — each with a
stated reason in `images/openldap-deb/debian/changelog` — and today its one
user is the [purge rotation](#the-accesslog-purge-rotation), whose per-pod
phase is a third field of `olcAccessLogPurge` that upstream does not accept.
Asking for it is a deliberate act, and the package revision tells you which
you got: `slapd -VV` reports `+slaptain<N>` or `+pure<N>`.

> **⚠ Changing `storageFormat` on a populated cluster is not an upgrade.** The
> two LMDB formats are mutually unreadable, so every pod crashloops on volumes
> it cannot open — and slapd's message for it says
> `MDB_INVALID: File is not an LMDB file … Restore from backup!`, which is
> wrong: the volume is healthy and restoring would be destructive. Worse, the
> pod then blocks its own replacement, so correcting the spec is necessary but
> not sufficient; you must delete the stuck pod by hand. The supported
> migration is a dump and reload. Read
> [`docs/OPENLDAP-VERSIONS.md`](docs/OPENLDAP-VERSIONS.md) **before** you touch
> this field.

## Key Features

- **Layered CRD architecture**: `SlapdMesh` describes the sites; `SlapdCluster` manages one site's infrastructure (StatefulSet, Services, TLS); `SlapdDatabase` manages per-database lifecycle (ACLs, indices, replication, seed data); `SlapdSchema` manages global schemas. Each layer references the one above it by name.
- **N-way multi-master replication**: all pods are symmetric read-write peers via delta-syncrepl. No permanent primary, no leader election. Per-database replication with user-controlled RID assignment.
- **Read-only consumer replicas**: scale read-heavy workloads without adding write complexity. RO pods consume from all RW masters for resilience.
- **Declarative ACLs and schemas**: declare ACL rules on `SlapdDatabase`, schema elements on `SlapdSchema`; the operator applies them to every pod's `cn=config` individually and self-heals after pod replacement.
- **Automatic credential management**: the operator generates per-database admin and replication passwords in Kubernetes Secrets — or reads them from user-provided Secrets.
- **Multi-database support**: run multiple independent databases (each with its own suffix, credentials, ACLs, replication config) in one cluster.
- **Pod placement**: `spec.placement` passes through the five Kubernetes knobs that
  decide where pods land — spread constraints, `nodeSelector`, node affinity,
  tolerations, priority class — and a cluster that declares none gets one generated
  spread constraint per fleet (`maxSkew: 1` over `kubernetes.io/hostname`,
  `ScheduleAnyway`). It matters more here than for a typical workload: the PVCs are
  RWO on local storage, so a pod is pinned to its volume's node for the life of the
  PVC and a bad spread is permanent. See [Pod Placement](docs/PLACEMENT.md).
- **Security**: rootless execution (UID 1024), no privilege escalation, read-only root filesystem, distroless runtime images, TLS encryption.
- **Database lifecycle**: cleanup policy (Retain/Delete) controls what happens when a `SlapdDatabase` CR is deleted. Default: Retain (database stays in slapd, becomes unmanaged).
- **Data-plane image selection**: `spec.images.select` names the build by what it governs — the on-disk format its volumes are in, and whether it carries slaptain's patches — and drives both the slapd and slapd-init images from one declaration, so a mismatched pair is not a state you can reach.
- **Accesslog purge rotation** (optional): stagger each pod's journal purge and move the write endpoint off the pod that is purging, so the purge never blocks the writes. See [The Accesslog Purge Rotation](#the-accesslog-purge-rotation).
- **Backup & restore**: on-demand and scheduled `slapcat`→S3 backups with retention (`SlapdBackup` / `SlapdScheduledBackup`); restore into a fresh database (`SlapdDatabase.spec.bootstrapFrom`) or roll back an existing one in place (`SlapdRestore`). See [Backup & Restore](docs/BACKUP.md).
- **Multi-site meshes**: describe the sites once in a `SlapdMesh` and apply one chart with one values file at every site; the operator derives each site's serverID stride slot, its cross-site peers, the network mode and the trust wiring from the mesh plus its own `siteName`. See [Multi-Site Deployments](docs/MULTI-SITE.md).
- **Diagnostics**: `slctl` inspects every pod directly and judges what it finds — CSN convergence per database, topology, stanza counts, suffix visibility — and collects a full incident bundle with `debug-dump`. See [slctl](docs/slctl.md).
- **Server-side apply**: all resource management uses SSA — no optimistic concurrency conflicts.

## The Accesslog Purge Rotation

Delta-syncrepl writes every change to a per-database journal, and that journal
has to be pruned. The problem is *where* the pruning happens:
`accesslog_purge()` deletes expired entries in an unpaced loop while holding
the journal environment's single LMDB writer — and every client write appends
to that same environment. **So a provider purging its own journal stalls its
own writes.** Measured at 100k ops/s: a 30-second write stall per cycle, with
reads continuing at roughly 75 %.

Scheduling alone cannot fix it. The duty cycle is `write rate / delete rate`
and the interval does not appear in that fraction, so a provider that takes
writes pays regardless of how often it purges.

The rotation moves the work instead of rescheduling it: every pod gets its own
purge phase on a shared grid, and a `<cluster>-write` Service follows one slot
behind, so **the pod taking writes is never the pod purging**. Measured with
the endpoint moving early enough for clients to drain, the per-boundary stall
is gone rather than smaller — 137,626 ms worst-case write latency became
2,314 ms.

It is **opt-in**, it needs clients that re-dial (a Service does not re-wire an
established connection), and the sizing is arithmetic you have to do for your
own write rate. It also needs the `slaptain` image flavour, because the per-pod
phase is a field upstream slapd does not accept.

```yaml
spec:
  images:
    select:
      flavour: slaptain
  rotation:
    enabled: true
    slotLength: 6h
```

Read [`docs/PURGE-ROTATION.md`](docs/PURGE-ROTATION.md) before enabling it — it
leads with the client-connection contract, which decides whether the feature
pays for you at all.

## Multi-Site Meshes

Multi-site is a **first-class feature**, not a deployment trick: one directory
across several Kubernetes clusters, replicating bidirectionally (N-way
multi-master delta-syncrepl) over TLS, with no shared API server and no shared
control plane. Each site stays autonomous — it keeps serving, and keeps being
reconfigurable, while the others are unreachable.

It has its own CRD. A `SlapdMesh` describes the sites once — each site's
`serverIDIndex`, how to reach it, how to trust it, and the network the
replication traffic rides. A `SlapdCluster` joins by name:

```yaml
spec:
  meshRef: slapd-mesh
```

and the operator derives that site's `serverIDBase`, its external peers, the
network mode and the trust wiring from the mesh plus its own `siteName`. Because
the mesh object carries no per-site fields, **the same chart and the same values
file are applied unchanged at every site** — the one argument that differs
anywhere is `--set siteName=<site>` on the operator's own chart.

Full guide: **[Multi-Site Deployments](docs/MULTI-SITE.md)** — the model, the
cross-site invariants, a walk-through install, and how to tell a mesh is
healthy. Chart: [`charts/slapd-mesh`](charts/slapd-mesh/README.md). Sample CR:
[`operator/config/samples/ldap_v1alpha1_slapdmesh.yaml`](operator/config/samples/ldap_v1alpha1_slapdmesh.yaml).
Design record: [ADR-028](docs/adrs/adr-028-mesh-scoped-vs-site-scoped.md).

Peers can still be written out by hand on a mesh-less `SlapdCluster`
(`spec.replication.externalPeers`, with a `uri`, static `podAddresses` or a
`discovery` block) — the lower-level form the mesh derives, and still the way to
peer with something that is *not* a slaptain cluster (see
[ADR-011](docs/adrs/adr-011-hot-migration-topology.md) on migration topologies).
The `discovery` form is what the e2e suite exercises; `uri` and static
`podAddresses` lost their end-to-end coverage when the mesh became the only test
path, and are carried as open items in [`docs/BACKLOG.md`](docs/BACKLOG.md).

RID scheme: each `SlapdDatabase` declares a `ridSegment` — the contiguous run of
replica IDs it owns. Within that run the first slots name pods at the local
site and the rest name peer sites, so `rid = 1 + segment·(P+S) + offset`. See
[ADR-003](docs/adrs/adr-003-operator-owns-syncrepl.md).

## Backup & Restore

Slaptain backs up each database's data tree to S3 (or any S3-compatible store) and restores it back. Artifacts are gzipped `slapcat` LDIF — interchangeable with stock `slapcat`/`slapadd`. Everything else (schemas, ACLs, replication topology) is reconstructed from the CRs, so a restore is "recreate the CRs, load the DIT."

```yaml
# On-demand backup of one database to S3
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdBackup
metadata:
  name: nightly
spec:
  databaseRef: myapp
  storage:
    bucket: my-ldap-backups
    region: eu-central-1
    credentialsSecretName: my-s3-creds   # keys: access-key-id, secret-access-key
```

| CRD | Purpose |
|---|---|
| `SlapdBackup` | One-shot, on-demand backup to S3 |
| `SlapdScheduledBackup` | Cron-scheduled backups with retention (`maxCount` / `maxAge`) |
| `SlapdDatabase.spec.bootstrapFrom` | Restore a backup into a **fresh** database (one-shot, mutually exclusive with `seed`) |
| `SlapdRestore` | Imperative **in-place** rollback of an existing, populated database |

Restores are *destroy-last* — the backup is validated (reachable, decompresses, correct suffix, replication-password matches) **before** any data is touched — and load every pod directly via offline `slapadd`, so a multi-replica restore needs no syncrepl refresh. Full guide: **[Backup & Restore](docs/BACKUP.md)**.

## Architecture

Slaptain splits the directory into the layers OpenLDAP itself has, rather than
one CR per deployment:

```
SlapdMesh  <--- meshRef ---  SlapdCluster  <--- clusterRef ---  SlapdDatabase
(sites, network, trust)      (one site)                         SlapdSchema
```

| CRD | Scope | Purpose |
|---|---|---|
| `SlapdMesh` | Multi-site | The site inventory: each site's `serverIDIndex`, the replication network, the trust wiring. Referenced by `meshRef`. |
| `SlapdCluster` | Infrastructure | StatefulSet, Services, PVCs, TLS, cn=config admin — one site's server fleet. |
| `SlapdDatabase` | Per-database | Suffix, credentials, ACLs, indices, replication, seed data. |
| `SlapdSchema` | Global schemas | attributeTypes, objectClasses applied to cn=schema,cn=config. |
| `SlapdBackup` | One-shot | A `slapcat` backup of one database to S3. |
| `SlapdScheduledBackup` | Recurring | Cron-scheduled backups with retention. |
| `SlapdRestore` | One-shot | In-place rollback of an existing, populated database. |

**Why this split rather than one big CR?** OpenLDAP natively supports multiple
independent databases per process, each with its own suffix, credentials, and
ACLs, while schemas are global. The CRD model mirrors that — no impedance
mismatch between the Kubernetes API and OpenLDAP's architecture
([ADR-004](docs/adrs/adr-004-multi-resource-crd-architecture.md)).

**Why the mesh is its own object.** Every mesh-scoped resource carries *no
per-site fields*, so the same YAML is applied byte-identically at every site and
cross-site parity is checkable as "does it exist and does it hash the same". The
one per-site fact in the whole system is the operator's own `siteName`
([ADR-028](docs/adrs/adr-028-mesh-scoped-vs-site-scoped.md)).

Each kind has its own controller; `SlapdCluster`'s also watches `SlapdDatabase`
CRs so data directories exist before databases are created, and drives the
restore machine.

## Why Slaptain?

Running a replicated OpenLDAP cluster on Kubernetes creates lifecycle problems that Helm charts and init scripts cannot solve:

- **cn=config is node-local**: OpenLDAP's runtime configuration is never replicated between pods. A pod replacement resets ACLs, schemas, and syncrepl stanzas. The operator detects and corrects drift on every reconcile loop.
- **Bootstrap sequencing matters**: delta-syncrepl requires the accesslog overlay to capture every write from the start. The operator seeds initial data via live LDAP connections (not `slapadd`) so the accesslog records it.
- **Multi-database coordination**: adding a database to a running cluster requires creating the `olcDatabase` entry in cn=config, setting up overlays, creating the replication bind user, and configuring syncrepl stanzas — all per-pod. The operator handles this declaratively.

## Documentation

- [Demo Walkthrough](docs/demo/README.md) — copy-paste walk through deploy, scale-up, backup and restore on one cluster
- [Team Onboarding](docs/ONBOARDING.md) — LDAP concepts, OpenLDAP specifics, operator model
- [Multi-Site Deployments](docs/MULTI-SITE.md) — the mesh model, the cross-site invariants, a multi-site install, and how to check a mesh is healthy
- [Bootstrap Internals](docs/BOOTSTRAP.md) — init container and operator bootstrap sequencing
- [Backup & Restore](docs/BACKUP.md) — S3 backup, scheduled backups + retention, restore into a fresh DB, in-place rollback
- [Purge Rotation](docs/PURGE-ROTATION.md) — the client-connection contract first, then how to turn the rotation on, the sizing arithmetic, and what it cannot do
- [Pod Placement](docs/PLACEMENT.md) — where a cluster's pods land: the generated per-fleet spread and why it is soft, the deadlock a hard constraint reaches with a bound local PV, and why cross-fleet separation is not expressible as a spread
- [Monitoring](docs/MONITORING.md) — what slaptain exposes to Prometheus, the join keys, and what it still cannot tell you
- [Upgrading](docs/UPGRADING.md) — what an operator upgrade does to a running mesh, per release
- [Tuning & Sizing](docs/TUNING.md) — what is tunable, slaptain's defaults and how they differ from slapd's, and sizing a cluster from lab to production
- [Versioning](docs/VERSIONING.md) — where the `v` goes (git tags) and where it must not (images, charts, `--version`)
- [TLS Certificates](docs/TLS.md) — where certificates come from (org PKI, cert-manager, the cluster CA), and how clients come to trust them
- [slctl](docs/slctl.md) — the diagnostic CLI: status, inspect, debug-dump, ldapsearch/add/modify/delete against a managed cluster
- [OpenLDAP Versions](docs/OPENLDAP-VERSIONS.md) — the 2.7/2.6 image pairs, the tag scheme, and the 2.6 -> 2.7 migration runbook
- [Sample manifests](operator/config/samples/) — one commented example per CRD
- [Architecture Decision Records](docs/adrs/) — ADR-001 through ADR-049
- [Development Guide](docs/DEVELOPMENT.md) — prerequisites, image builds, operator dev loop, e2e cycle, debugging, project discipline
- [GitHub Issues](https://github.com/chuck-chuck-chuck-net/slaptain/issues) — bug reports and feature requests

## Development

```bash
git clone https://github.com/chuck-chuck-chuck-net/slaptain.git
cd slaptain

# Build all images
make all

# Regenerate deepcopy and CRD after type changes
make operator-generate operator-manifests

# Deploy operator via Helm
make operator-helm-install

# Deploy a SlapdCluster + test resources
make cluster-helm-install testing-apply

# Run e2e tests
make e2e-run

# Or: full setup/test/teardown via NodePort (single command). One context is a
# single-site run; two or more stand up a real multi-site mesh, one logical site
# per context. Omit it entirely to use the current kubectl context.
./tests/e2e.sh all [kubectl-context...]

# Package and push the charts (needs `helm registry login` first)
make charts-package
make charts-push
```

Sample manifests for every CRD: [`operator/config/samples/`](operator/config/samples/).
Full CRD type definitions: [`operator/api/v1alpha1/`](operator/api/v1alpha1/).

## License

Apache License 2.0
