# Slaptain

[![GitHub](https://img.shields.io/badge/github-chuck--chuck--chuck--net%2Fslaptain-blue?logo=github)](https://github.com/chuck-chuck-chuck-net/slaptain)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

A Kubernetes operator for deploying OpenLDAP (slapd) as a highly available, replicated directory service.

Slaptain manages the full lifecycle of multi-master OpenLDAP clusters on Kubernetes: N-way delta-syncrepl replication, per-database ACLs and custom schemas, read-only consumer replicas, and cross-cluster peering. It provides the control plane that static Helm charts and bootstrap scripts cannot — self-healing configuration, declarative database management, and proper bootstrap sequencing.

## Quick Start

### 1. Install the Operator

```bash
helm upgrade --install slaptain-operator oci://ghcr.io/chuck-chuck-chuck-net/charts/slaptain-operator \
  -n slaptain-system --create-namespace
```

This installs the CRDs (`SlapdCluster`, `SlapdDatabase`, `SlapdSchema`), RBAC, and operator Deployment.

### 2. Deploy a Cluster

Create a TLS Secret and a `SlapdCluster` CR (infrastructure only — no databases yet):

```bash
# Generate a self-signed TLS cert (or use your own)
kubectl create namespace slaptain
make gencert

# Single replica (simplest)
kubectl apply -f operator/config/samples/ldap_v1alpha1_slapdcluster.yaml
```

For a replicated cluster, set `replicas` and `replication.enabled`:

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdCluster
metadata:
  name: slapd
spec:
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
    keepalive: "300:10:60"

  readReplicas: 1

  persistence:
    enabled: true
    config:
      size: 1Gi
    data:
      size: 5Gi
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
    ridBase: 100
    deltaSync: true
    syncprovCheckpoint: "500 15"

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

## Key Features

- **Multi-resource CRD architecture**: `SlapdCluster` manages infrastructure (StatefulSet, Services, TLS); `SlapdDatabase` manages per-database lifecycle (ACLs, indices, replication, seed data); `SlapdSchema` manages global schemas. Clean separation of concerns.
- **N-way multi-master replication**: all pods are symmetric read-write peers via delta-syncrepl. No permanent primary, no leader election. Per-database replication with user-controlled RID assignment.
- **Read-only consumer replicas**: scale read-heavy workloads without adding write complexity. RO pods consume from all RW masters for resilience.
- **Declarative ACLs and schemas**: declare ACL rules on `SlapdDatabase`, schema elements on `SlapdSchema`; the operator applies them to every pod's `cn=config` individually and self-heals after pod replacement.
- **Automatic credential management**: the operator generates per-database admin and replication passwords in Kubernetes Secrets — or reads them from user-provided Secrets.
- **Multi-database support**: run multiple independent databases (each with its own suffix, credentials, ACLs, replication config) in one cluster.
- **Security**: rootless execution (UID 1024), no privilege escalation, read-only root filesystem, distroless runtime images, TLS encryption.
- **Database lifecycle**: cleanup policy (Retain/Delete) controls what happens when a `SlapdDatabase` CR is deleted. Default: Retain (database stays in slapd, becomes unmanaged).
- **Backup & restore**: on-demand and scheduled `slapcat`→S3 backups with retention (`SlapdBackup` / `SlapdScheduledBackup`); restore into a fresh database (`SlapdDatabase.spec.bootstrapFrom`) or roll back an existing one in place (`SlapdRestore`). See [Backup & Restore](docs/BACKUP.md).
- **Server-side apply**: all resource management uses SSA — no optimistic concurrency conflicts.

## Cross-Cluster Replication

Slaptain supports cross-cluster replication via `spec.replication.externalPeers` on the `SlapdCluster` CR. Each peer is an independent SlapdCluster running in a separate Kubernetes cluster. Replication is bidirectional (N-way multi-master) using simple bind over TLS.

```yaml
replication:
  enabled: true
  keepalive: "300:10:60"
  externalPeers:
    - name: site-b
      uri: "ldaps://ldap.site-b.example.com:636"
      tlsSecretName: "site-b-ca"
      bindDN: "cn=replication,dc=example,dc=org"
      bindPasswordSecretName: "site-b-repl-pw"
```

RID scheme: each `SlapdDatabase` declares a `ridBase`. In-cluster peers use RIDs `ridBase+1..ridBase+49`, external peers use `ridBase+51..ridBase+99`. See [ADR-003](docs/adrs/adr-003-operator-owns-syncrepl.md).

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

Slaptain uses three Custom Resource Definitions:

| CRD | Scope | Purpose |
|---|---|---|
| `SlapdCluster` | Infrastructure | StatefulSet, Services, PVCs, TLS, cn=config admin |
| `SlapdDatabase` | Per-database | Suffix, credentials, ACLs, indices, replication, seed data |
| `SlapdSchema` | Global schemas | attributeTypes, objectClasses applied to cn=schema,cn=config |

Each has its own controller. The `SlapdCluster` controller watches `SlapdDatabase` CRs to ensure data directories exist before databases are created. See [ADR-004](docs/adrs/adr-004-multi-resource-crd-architecture.md) for the full design rationale.

**Why three CRDs?** OpenLDAP natively supports multiple independent databases per process, each with its own suffix, credentials, and ACLs. Schemas are global (visible to all databases). The CRD model mirrors this — no impedance mismatch between the Kubernetes API and OpenLDAP's architecture.

## Why Slaptain?

Running a replicated OpenLDAP cluster on Kubernetes creates lifecycle problems that Helm charts and init scripts cannot solve:

- **cn=config is node-local**: OpenLDAP's runtime configuration is never replicated between pods. A pod replacement resets ACLs, schemas, and syncrepl stanzas. The operator detects and corrects drift on every reconcile loop.
- **Bootstrap sequencing matters**: delta-syncrepl requires the accesslog overlay to capture every write from the start. The operator seeds initial data via live LDAP connections (not `slapadd`) so the accesslog records it.
- **Multi-database coordination**: adding a database to a running cluster requires creating the `olcDatabase` entry in cn=config, setting up overlays, creating the replication bind user, and configuring syncrepl stanzas — all per-pod. The operator handles this declaratively.

## Documentation

- [Team Onboarding](docs/ONBOARDING.md) — LDAP concepts, OpenLDAP specifics, operator model
- [Bootstrap Internals](docs/BOOTSTRAP.md) — init container and operator bootstrap sequencing
- [Backup & Restore](docs/BACKUP.md) — S3 backup, scheduled backups + retention, restore into a fresh DB, in-place rollback
- [Architecture Decision Records](docs/adrs/) — ADR-001 through ADR-022
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

# Or: full setup/test/teardown via NodePort (single command; omit the
# context to use the current kubectl context)
./tests/e2e.sh all [kubectl-context]
```

Full CRD type definitions: [`operator/api/v1alpha1/`](operator/api/v1alpha1/).

## License

Apache License 2.0
