# ADR-002: cn=config Is Node-Local; the Operator Manages It

**Status:** Accepted
**Date:** 2026-03-01

## Context

OpenLDAP's runtime configuration database (`cn=config`) is stored on the local filesystem of
each pod in `/ldap-config`. Unlike the main data database, **cn=config is not replicated by
default** — each pod's configuration is entirely independent.

This creates a consistency problem in a multi-pod deployment. Any change to ACLs, schemas, or
overlays made on one pod (e.g. via `ldapmodify` against pod-0's `cn=config`) leaves every other
pod's `cn=config` unchanged. After a pod is replaced by Kubernetes, the new pod re-runs the
init container and starts with only the defaults from `slapd.conf`, losing any earlier
`ldapmodify` changes.

The original slapd-test bootstrap job worked around this by connecting to the ClusterIP Service
and issuing `ldapmodify` calls to apply ACLs. Because the ClusterIP load-balances across pods,
this could only guarantee that *one* pod (chosen at random) received the change. In a 3-replica
cluster this reliably left two pods with incorrect ACLs, causing intermittent `err=50`
(Insufficient Access) errors on anonymous searches.

## Options Considered

### A — Replicate cn=config

OpenLDAP supports replicating cn=config itself using `syncprov` on the `cn=config` database.
A designated "provider" pod pushes its cn=config to all "consumer" pods.

**Rejected because:**
- Requires designating one pod as the cn=config provider. This breaks the N-way multi-master
  symmetry: pod-0 becomes special again, which is exactly what we chose mirrormode to avoid.
- Consumer pods do not accept direct `ldapmodify` writes to `cn=config` while they are acting
  as consumers — all changes must go through the provider, or replication breaks.
- A cn=config provider pod becoming unavailable blocks all ACL and schema changes cluster-wide.
- Adds operational complexity (bootstrap order, provider designation, failover) that conflicts
  with the operator's goal of keeping all pods symmetric and self-healing.

### B — Apply ACLs from the slapd-test bootstrap Job (status quo)

The test job connects via ClusterIP and runs `ldapmodify` to replace ACLs.

**Rejected because:**
- ClusterIP load-balances randomly across pods; applying once reaches exactly one pod.
- Mixes concerns: a testing tool manages production `cn=config` state.
- Pod replacement (e.g. node failure, rollout) regenerates the pod from slapd.conf, erasing
  any `ldapmodify` changes. The test job is a one-shot Job; it does not re-run on pod restart.
- Cannot guarantee consistency across replicas.

### C — Per-Pod ACL Application from the Bootstrap Job

Extend the test job to connect to each pod individually via headless service DNS
(`slapd-0.slapd-headless`, etc.) and apply ACLs to every pod.

**Rejected because:**
- Still mixes concerns (test chart managing production state) and runs only once (not
  self-healing on pod replacement).
- The bootstrap job would need to know the replica count, headless service name, and config
  admin credentials — coupling test tooling to the operator's internal resource naming.

### D — Operator-Managed cn=config (chosen)

Declare desired ACL rules in `spec.ldap.acls` on the `SlapdCluster` CR. The operator applies
them to every pod's `cn=config` individually, using the headless service DNS
(`<name>-<ordinal>.<name>-headless.<ns>.svc.cluster.local`) to address each pod directly.

**Accepted because:**
- The operator knows the replica count, headless service name, and config admin credentials.
  It is the only component that can reliably reach *all* pods with the correct credentials.
- Self-healing: on every reconcile loop, the operator checks whether each pod's `olcAccess`
  values match the desired rules and patches them if not. Pod replacement or init-container
  re-run is automatically corrected on the next reconcile (≤10 seconds later).
- Single source of truth: ACL rules live in the CR spec alongside all other configuration.
  `kubectl describe slapdcluster slapd` shows the intended ACLs; diff against a pod's live
  cn=config is the authoritative view of drift.
- No new concepts needed: the operator already uses go-ldap to connect to slapd for bootstrap.
  The `reconcileACLs` step reuses the same dial/bind/modify pattern.
- Consistent with the Kubernetes operator model: declare desired state in a CR, controller
  continuously reconciles actual state toward it.

## Decision

Implement `spec.ldap.acls` as an ordered list of slapd.conf `access to ...` rules (without
the `{N}` ordinal prefix). The operator applies them atomically via a single `Modify` with
`Replace: olcAccess` on the data database's `cn=config` entry.

**Idempotency:** Before issuing a modify, the operator reads the current `olcAccess` values and
strips the `{N}` prefix that slapd adds internally. If the stripped values match the desired
rules in order, no modify is issued.

**Failure handling:** A pod that is not yet ready (init container still running, TCP not
accepting) causes the `reconcileACLs` call to log a warning and continue. The ACL will be
applied on the next reconcile once the pod is reachable. This prevents a single unhealthy pod
from blocking the reconcile loop.

**When `spec.ldap.acls` is empty (the default):** The operator does not touch `cn=config`. The
default ACLs from the init container's `slapd.conf` template remain in effect:
```
access to attrs=userPassword
  by self write
  by anonymous auth
  by * none

access to *
  by dn.exact="cn=replication,<domain>" read
  by * read
```

## Implementation

Added in `operator/api/v1alpha1/slapdcluster_types.go`:
```go
// ACLs is the list of OpenLDAP ACL rules (olcAccess entries) to apply to the
// data database on every pod. Rules are in standard slapd.conf "access to ..."
// format, without the {N} index prefix.
// +optional
ACLs []string `json:"acls,omitempty"`
```

Reconcile step 7 in `operator/internal/controller/slapdcluster_controller.go`:
- `reconcileACLs` — iterates over pod ordinals 0..replicas-1, dials each pod via headless DNS,
  binds as `cn=admin,cn=config` using `<name>-config-password` / `root-password`, finds the
  data DB entry via `(olcSuffix=<domain>)` search, compares current `olcAccess` values, and
  replaces if different.

## Consequences

- The slapd-test bootstrap chart no longer manages cn=config ACLs. The `readpwAclJson` value
  and the corresponding configmap key have been removed from `charts/slapd-test`.
- The config admin password (`<name>-config-password`) is now actively used from the first
  deployment (Phase 2), not deferred to Phase 3.
- Schema extensions (`spec.ldap.schemas`) require the same treatment: the operator must apply
  custom schemas to each pod's `cn=config` individually. This is tracked as a Phase 3 backlog
  item; in the interim, the slapd-test chart's `customSchemaJson` path continues to apply
  schemas to one pod (limited to single-pod or same-pod scenarios only).
- Any developer adding a reconcile step that modifies `cn=config` must read ADR-001 (double
  reconciliation is harmless) and implement idempotency as described here.

## Amendment (2026-04-19): Desired State Relocates to SlapdDatabase and SlapdSchema

The core decision (cn=config is node-local; the operator manages it per-pod via headless DNS)
is unchanged. What changes with ADR-004 is **where** the desired state is declared:

| Before (single CRD) | After (multi-resource) |
|---|---|
| `SlapdCluster.spec.ldap.acls` | `SlapdDatabase.spec.acls` (per-database) |
| `SlapdCluster.spec.ldap.schemas` | `SlapdSchema` CR (separate resource) |

The per-pod application pattern remains identical: each controller connects to each pod
individually via headless DNS, binds as `cn=admin,cn=config`, and applies the desired state.
The only difference is that the desired state comes from multiple CRs instead of one.

The `SlapdDatabase` controller applies ACLs to the specific `olcDatabase` entry matching its
suffix (found via `findDataDBDN()`). The `SlapdSchema` controller applies schemas to
`cn=schema,cn=config`. Both follow the idempotency pattern established in this ADR.

See ADR-004 for the full architecture and ADR-006 for schema-specific lifecycle semantics.

## Related

- ADR-001: Double reconciliation runs are harmless — explains why concurrent/repeated
  `reconcileACLs` calls are safe.
- ADR-004: Multi-resource CRD architecture — relocates ACLs and schemas to dedicated CRDs.
- ADR-006: Schema lifecycle — additive-only model for schema management.
- `docs/ONBOARDING.md` — see "ACLs" section and "The Operator Model" section for the
  user-facing explanation.
