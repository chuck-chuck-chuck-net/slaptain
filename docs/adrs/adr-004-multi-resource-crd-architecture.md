# ADR-004: Multi-Resource CRD Architecture

**Status:** Proposed
**Date:** 2026-04-19

## Context

The operator currently manages everything through a single `SlapdCluster` CR: infrastructure
(StatefulSet, Services, TLS), data databases (one, via `spec.ldap.domain`), ACLs
(`spec.ldap.acls`), custom schemas (`spec.ldap.schemas`), and replication. The `slapd-test`
Helm chart fills remaining gaps: bootstrap data seeding, additional schemas, and a debug toolkit.

This conflation of concerns causes several problems:

1. **Single database limitation.** OpenLDAP natively supports multiple independent data
   databases per process, each with its own suffix, credentials, ACLs, replication config,
   and indices. Real-world deployments commonly run 2-5 databases (e.g. separate trees for
   user data, management data, and signing config). The current CRD has a single
   `spec.ldap.domain` field and cannot represent this.

2. **Mixed abstractions in `SlapdCluster`.** ACLs and schemas are per-database concerns (ACLs
   are defined on the database entry in cn=config; schemas are consumed by specific databases),
   but are currently declared at the cluster level. This works for one database; it breaks
   down with multiple databases where each needs different ACLs.

3. **slapd-test chart is a grab bag.** It mixes four unrelated concerns: a bootstrap Job
   (data seeding), a toolkit Deployment (debug pod), password Secrets (test credentials), and
   schema application (cn=config modification). It was designed as a "get things done quickly"
   vehicle and lacks proper abstraction.

4. **Schema management has no proper CRD.** Schemas are global to the slapd process
   (stored in `cn=schema,cn=config`) but are currently an opaque JSON blob in
   `spec.ldap.schemas`. There is no ordering, no status tracking, no structured validation.

## Decision

Split the single `SlapdCluster` CRD into three resources:

### SlapdCluster — Infrastructure Only

Manages the physical cluster: StatefulSet, Services, PVCs, TLS, cn=config admin credentials.
**Does not create any data databases, schemas, or ACLs.**

The init container simplifies to:
- Set up cn=config with admin credentials
- Load modules (`back_mdb`, `syncprov`, `accesslog`)
- Configure TLS
- No data database, no schemas, no ACLs, no replication stanzas

A cluster with zero `SlapdDatabase` resources is valid — pods run slapd with only cn=config.

Key spec fields:
- `images`, `replicas`, `readReplicas`
- `persistence` (config, data, accesslog PVC sizes — shared by all databases)
- `tls`, `service`, `resources`, `securityContext`
- `replication` (global settings: enabled, externalPeers, keepalive, retry)
- `cnConfigCredentials` (cn=config admin credentials)

Removed from current `SlapdCluster`:
- `spec.ldap.domain` — no default database
- `spec.ldap.acls` — moves to `SlapdDatabase`
- `spec.ldap.schemas` — moves to `SlapdSchema`
- `spec.ldap.credentialsSecretName` — replaced by `spec.cnConfigCredentials`

### SlapdSchema — Global Schema Definitions

Schemas are stored in `cn=schema,cn=config` and are global — visible to all databases on the
slapd process. Making them a separate CR provides:

- **Structured format:** `attributeTypes` and `objectClasses` as typed lists, not opaque JSON.
- **Ordering:** A `priority` field controls application order (lower = first). Built-in schemas
  (core, cosine, nis, inetorgperson) are loaded by the init container. Custom schemas start at
  higher priorities.
- **Per-pod status tracking:** The controller reports which pods have the schema applied.
- **Composability:** Different `SlapdDatabase` resources may require different schemas. Each
  schema is an independent CR that can be deployed, versioned, and managed separately.

The controller applies schemas to every pod individually (cn=config is node-local, per ADR-002).

### SlapdDatabase — Per-Database Configuration

Each instance represents one MDB backend database within a cluster. This is where all
application-level configuration lives:

- `suffix` (e.g. `o=myapp`)
- `rootDN` (defaults to `cn=admin,<suffix>`)
- `credentials` (per-database root password)
- `acls` (per-database ACL rules)
- `indices`
- `replication` config (deltaSync, syncprov checkpoint, accesslog purge, RID base)
- `seed` (initial LDIF entries, applied once)
- `cleanupPolicy` (Retain or Delete; see ADR-005)

The controller:
1. Creates the database via `ldapmodify` on each pod's cn=config (adding an `olcDatabase` entry)
2. Configures ACLs, indices, overlays per-database
3. Configures syncrepl stanzas per-database (using the database's `ridBase`)
4. Seeds initial data (one-time, tracked in status)

### Shared Data PVC with Subdirectories

All databases share one data PVC per pod (`/ldap-data/`). Each database gets a subdirectory
(e.g. `/ldap-data/myapp/`, `/ldap-data/mgmt/`). This avoids the complexity of dynamic PVC
provisioning and follows the pattern used by other database operators (e.g. mariadb-operator).

The `dataDirectory` field on `SlapdDatabase` controls the subdirectory name (defaults to a
sanitized version of the suffix).

### Controller Architecture

Three controllers, with reconcile ordering enforced by status dependencies:

1. **SlapdCluster controller** — StatefulSet, Services, cn=config credentials. Sets
   `status.phase = Running` when pods are up with cn=config accessible.
2. **SlapdSchema controller** — Watches `SlapdSchema` CRs. Waits for referenced cluster to
   be Running. Applies schemas to all pods in priority order. Idempotent.
3. **SlapdDatabase controller** — Watches `SlapdDatabase` CRs. Waits for referenced cluster
   to be Running and required schemas to be applied. Creates databases, configures ACLs,
   indices, replication, seeds data.

All three controllers follow ADR-001 (idempotent reconciliation) and ADR-002 (per-pod cn=config
management via headless DNS).

## Alternatives Considered

### Keep a "default database" on SlapdCluster

Having `SlapdCluster` always create one database from `spec.ldap.domain` would simplify
single-database deployments. Rejected because:
- It creates an asymmetry: the first database is different from all subsequent ones.
- Users must learn two different models (cluster-level database vs CR-level database).
- The additional YAML to create a `SlapdDatabase` CR is trivial.

### Embed schemas in SlapdDatabase

Declaring schemas on the database that needs them (e.g. `SlapdDatabase.spec.schemas`).
Rejected because:
- Schemas are global in OpenLDAP — they live in cn=schema,cn=config and are visible to all
  databases. Declaring them on one database is misleading.
- Two databases needing the same schema would duplicate the definition.
- Schema ordering/dependencies are a separate concern from database configuration.

### Keep schemas as an opaque JSON blob

The current `spec.ldap.schemas` approach. Rejected because:
- No validation — a typo in an OID is only caught at runtime.
- No diffing — you can't see what changed between two versions of the CR.
- The structured format (`attributeTypes`, `objectClasses`) matches how OpenLDAP actually
  stores schemas in cn=config and how schema RFCs define them.

## Consequences

- **Breaking change.** The `SlapdCluster` CRD changes significantly (removed fields). This is
  acceptable at v1alpha1.
- **slapd-test chart is replaced.** Its responsibilities decompose into `SlapdSchema` CRs
  (custom schemas), `SlapdDatabase` seed data (OUs, service users), and `slctl shell`
  (debug toolkit). The chart is deprecated and eventually removed.
- **Init container simplifies.** No longer generates data databases, schemas, ACLs, or
  replication stanzas. Only cn=config infrastructure.
- **Three controllers instead of one.** More code, but each controller has a single
  responsibility and is independently testable.
- **Migration path needed.** Existing deployments have a single database baked into the
  StatefulSet's init container. A migration guide must document how to transition to the
  new model.

## Related

- ADR-001: Double reconciliation — all three controllers must be idempotent.
- ADR-002: cn=config is node-local — established the per-pod management pattern used by all
  three controllers.
- ADR-003: Operator owns syncrepl — extended to per-database syncrepl with user-provided
  RID bases (see amendment).
- ADR-005: SlapdDatabase cleanup policy.
- ADR-006: Schema lifecycle and update semantics.
