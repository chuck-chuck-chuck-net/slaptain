# Slaptain

[![GitHub](https://img.shields.io/badge/github-chuck--chuck--chuck--net%2Fslaptain-blue?logo=github)](https://github.com/chuck-chuck-chuck-net/slaptain)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)

A Kubernetes operator for deploying OpenLDAP (slapd) as a highly available, replicated directory service.

Slaptain is built for workloads where LDAP is the source of truth for user identities across multiple applications — mail (Dovecot), identity (Keycloak), groupware (OX App Suite). It provides N-way multi-master replication with delta-syncrepl, operator-managed ACLs, and read-only consumer replicas: the class of problem where static Helm charts and bootstrap scripts reach their limits.

## Quick Start

### 1. Install the Operator

```bash
helm install slaptain-operator ./charts/operator \
  -n slaptain-system --create-namespace
```

This installs the CRD, RBAC, and operator Deployment.

### 2. Deploy a Cluster

Create a TLS Secret and a `SlapdCluster` CR:

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
    domain: "dc=example,dc=org"
    tls:
      enabled: true
      secretName: slapd-tls

  replicas: 3
  replication:
    enabled: true

  # Optional: read-only consumer replicas for scaling reads
  readReplicas: 1

  persistence:
    enabled: true
    config:
      size: 1Gi
    data:
      size: 5Gi
```

### 3. Verify Health

```bash
kubectl get slapdcluster
# NAME    PHASE     READY   REPLICAS   AGE
# slapd   Running   3       3          2m

# With -o wide to see read-only replica status
kubectl get slapdcluster -o wide
```

## Key Features

- **N-way multi-master replication**: all pods are symmetric read-write peers via delta-syncrepl. No permanent primary, no leader election — any pod can serve reads and writes. Pod failure is handled gracefully.
- **Read-only consumer replicas**: scale read-heavy workloads (auth lookups, address book queries) without adding write complexity. RO pods consume from all RW masters for resilience.
- **Operator-managed ACLs**: declare ACL rules once in `spec.ldap.acls`; the operator applies them to every pod's `cn=config` individually and self-heals after pod replacement.
- **Operator-managed schemas**: declare custom LDAP schemas in `spec.ldap.schemas`; the operator adds them to every pod's `cn=schema,cn=config` idempotently.
- **Automatic credential management**: the operator generates and manages admin, config admin, and replication passwords in Kubernetes Secrets — or reads them from a user-provided Secret.
- **Security**: rootless execution (UID 1024), no privilege escalation, read-only root filesystem, distroless runtime images, TLS encryption.
- **Persistent storage**: per-pod PVCs via StatefulSet `volumeClaimTemplates` for config, data, and accesslog volumes.
- **Server-side apply**: all resource management uses SSA — no optimistic concurrency conflicts, no accidental field overwrites.

## Cross-Cluster Replication

Slaptain supports cross-cluster replication via `spec.replication.externalPeers`. Each peer is an independent SlapdCluster running in a separate Kubernetes cluster. Replication is bidirectional (N-way multi-master) using simple bind over TLS.

Each cluster's slapd TLS certificate is reused as the mTLS client certificate. The peer's CA is provided via `tlsSecretName` so each side can verify the other.

```yaml
replication:
  enabled: true
  externalPeers:
    - name: site-b
      uri: "ldaps://ldap.site-b.example.com:636"
      tlsSecretName: "site-b-ca"           # Secret with peer's ca.crt
      bindDN: "cn=replication,dc=example,dc=org"
      bindPasswordSecretName: "site-b-repl-pw"  # Secret with 'password' key
```

The operator manages all syncrepl stanzas (in-cluster + external) at runtime. Adding or removing external peers from the spec triggers a reconcile that updates `cn=config` on every pod — no pod restarts needed. External peer connectivity is reported in `.status.externalPeerStatuses`.

RID scheme: in-cluster peers use RIDs 1..99 (skip self), external peers use RIDs 101+. See [ADR-003](docs/adrs/adr-003-operator-owns-syncrepl.md) for the full design rationale.

## Configuration Reference

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdCluster
metadata:
  name: slapd
spec:
  images:
    slapd:
      repository: registry.example.com/slaptain/slapd
      tag: latest
      pullPolicy: IfNotPresent
    init:
      repository: registry.example.com/slaptain/slapd-init
      tag: latest
      pullPolicy: IfNotPresent

  ldap:
    domain: "dc=example,dc=org"
    credentialsSecretName: ""     # Optional: supply your own passwords
    forceRebootstrap: false       # Destructive: wipe and re-bootstrap
    tls:
      enabled: true
      secretName: slapd-tls       # Must contain tls.crt, tls.key, ca.crt
    acls:                         # Ordered "access to ..." rules; empty = defaults
      - 'to attrs=userPassword by self write by anonymous auth by * none'
      - 'to * by * read'
    schemas: []                   # JSON-encoded schema entries for cn=schema,cn=config

  replicas: 3                     # RW replicas (replication.enabled=true for >1)
  readReplicas: 0                 # RO consumer replicas (requires replication)
  logLevel: 0                    # slapd -d debug level

  replication:
    enabled: true
    # Cross-cluster peers (Phase 3)
    externalPeers:
      - name: site-b
        uri: "ldaps://ldap.site-b.example.com:636"
        tlsSecretName: "site-b-ca"
        bindDN: "cn=replication,dc=example,dc=org"
        bindPasswordSecretName: "site-b-replication-password"

  persistence:
    enabled: true
    config:
      size: 1Gi
      storageClass: ""
      accessMode: ReadWriteOnce
    data:
      size: 5Gi
      storageClass: ""
      accessMode: ReadWriteOnce
    accesslog:                    # Only provisioned when replication is enabled
      size: 1Gi
      storageClass: ""
      accessMode: ReadWriteOnce

  service:
    type: ClusterIP
    ldapPort: 389
    ldapsPort: 636

  resources: {}
  # resources:
  #   requests: { cpu: 100m, memory: 128Mi }
  #   limits: { memory: 512Mi }
```

Full CRD type definitions: [`operator/api/v1alpha1/slapdcluster_types.go`](operator/api/v1alpha1/slapdcluster_types.go).

## Why Slaptain?

Running a replicated OpenLDAP cluster on Kubernetes creates lifecycle problems that Helm charts and init scripts cannot solve on their own.

**The problem:** OpenLDAP's `cn=config` (the runtime configuration database) is node-local — it is never replicated between pods. A pod replacement re-runs the init container, resetting ACLs and syncrepl stanzas to their generated defaults. Meanwhile, delta-syncrepl requires careful bootstrap sequencing: the accesslog overlay must capture every write from the start, or consumers get "provider has no state info" errors that require a full re-bootstrap.

**The solution:** Slaptain treats these as control-plane responsibilities:

1. **Bootstrap sequencing**: the operator waits for pod-0, seeds the initial directory entries via a live LDAP connection (so the accesslog captures them), and lets subsequent pods sync from it.
2. **ACL convergence**: `spec.ldap.acls` is applied to every pod's `cn=config` on every reconcile loop — drift is detected and corrected automatically, including after pod replacement.
3. **Read-only scaling**: a second StatefulSet of pure consumer pods pulls from all RW masters without running syncprov or mirrormode, so they never accept writes and cannot corrupt the replication topology.

The core principle is **let slapd do what it does well** (LMDB storage, syncrepl protocol, ACL evaluation) and manage the Kubernetes-specific lifecycle around it.

## Documentation

- [Team Onboarding](docs/ONBOARDING.md) — LDAP concepts, OpenLDAP specifics, operator model, credential model
- [Bootstrap Internals](docs/BOOTSTRAP.md) — init container and operator bootstrap sequencing
- [Architecture Decision Records](docs/adrs/) — design rationale for double reconciliation, cn=config management
- [GitHub Issues](https://github.com/chuck-chuck-chuck-net/slaptain/issues) — bug reports and feature requests

## Development

```bash
git clone https://github.com/chuck-chuck-chuck-net/slaptain.git
cd slaptain

# Build all images
make all

# Build + push all images
make push

# Regenerate deepcopy and CRD after type changes
make operator-generate operator-manifests

# Deploy operator via Helm
make operator-helm-install

# Deploy a SlapdCluster + test harness
make cluster-helm-install testing-helm-install

# Run e2e tests (44 specs: base + replication + resilience + read-only)
make e2e-run

# Run e2e tests in-cluster
make e2e-in-cluster
```

See [tests/README.md](tests/README.md) for the full test cycle.

## License

Apache License 2.0
