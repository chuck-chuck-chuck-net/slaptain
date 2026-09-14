# ADR-005: SlapdDatabase Cleanup Policy

**Status:** Proposed
**Date:** 2026-04-19

## Context

When a `SlapdDatabase` CR is deleted from the cluster, the operator must decide what to do with
the corresponding data:

1. The `olcDatabase` entry in each pod's cn=config (the database definition)
2. The LMDB data files on disk (the actual directory data, under `/ldap-data/<subdir>/`)

Deleting LDAP data is **irreversible** in a way that deleting a Kubernetes resource is not.
`kubectl delete slapddatabase mydb` is a single command; recovering the directory data requires
restoring from backup or re-seeding from a peer (if replication is still active somewhere).

This is a well-known problem in database operators. The mariadb-operator addresses it with a
`cleanupPolicy` field on their `Database` CR:

- `Retain` (default) — the database definition and data survive CR deletion.
- `Delete` — the database definition and data are removed after CR deletion.

## Decision

`SlapdDatabase` has a `spec.cleanupPolicy` field with two values:

### Retain (default)

When the `SlapdDatabase` CR is deleted:
- The operator removes its finalizer and allows the CR to be garbage-collected.
- The `olcDatabase` entry in cn=config is **not removed** from any pod.
- The data files under `/ldap-data/<dataDirectory>/` are **not deleted**.
- Syncrepl stanzas for this database are removed (the operator no longer manages them).
- The database continues to function — slapd serves reads and accepts writes — but it is
  now **unmanaged**. ACL drift, schema drift, and replication drift are no longer corrected.

To re-adopt an unmanaged database, create a new `SlapdDatabase` CR with the same `suffix`.
The controller detects the existing `olcDatabase` entry and reconciles it to the desired state
instead of creating a new one.

### Delete

When the `SlapdDatabase` CR is deleted:
- The operator removes the `olcDatabase` entry from each pod's cn=config via `ldapmodify`.
- The operator does **not** delete data files from disk. LMDB files in
  `/ldap-data/<dataDirectory>/` are orphaned. This is intentional: the operator should not
  perform destructive filesystem operations inside another pod's PVC. Data cleanup is a
  storage/admin concern.
- Syncrepl stanzas for this database are removed.
- The database stops being served by slapd immediately on each pod where the cn=config
  entry is removed.

**Why not delete data files?** The operator runs as a separate pod and interacts with slapd
via LDAP over the network. It has no filesystem access to the slapd pods' PVCs. Deleting
files would require `kubectl exec` or a sidecar — both of which are rejected patterns in
this project (see credential architecture in CLAUDE.md). Orphaned data files consume disk
space but pose no correctness risk; they can be cleaned up by deleting the PVC or by an
admin running a manual cleanup.

## Implementation

The controller uses a **finalizer** (`ldap.chuck-chuck-chuck.net/database-cleanup`) to
intercept CR deletion:

1. CR deletion detected (`.metadata.deletionTimestamp` is set).
2. If `cleanupPolicy == Delete`: remove `olcDatabase` entry from each reachable pod's cn=config.
3. If `cleanupPolicy == Retain`: no-op (database stays in cn=config).
4. In both cases: remove syncrepl stanzas for this database from all pods.
5. Remove the finalizer. CR is garbage-collected.

Unreachable pods (not ready, network partition) are skipped with a warning. The finalizer is
not removed until all reachable pods have been processed. If a pod is permanently unreachable,
the finalizer blocks CR deletion — this is intentional, as it prevents inconsistent state. An
admin can manually remove the finalizer to force deletion.

## Consequences

- Default behavior is safe: `kubectl delete slapddatabase mydb` does not destroy data.
- Explicit opt-in for cleanup: `spec.cleanupPolicy: Delete` must be set before deletion.
- Re-adoption of unmanaged databases is supported (create CR with same suffix).
- Orphaned data files under `Delete` policy must be cleaned up manually or by PVC deletion.
- Finalizer may block CR deletion if pods are unreachable — admin can force by removing
  the finalizer.

## Amendment (2026-09-14): what "remove the olcDatabase entry" actually takes

The implementation did not match this ADR, in the one case the policy exists for.
`deleteDatabaseFromPod` issued a bare `Del` of the data database's
`olcDatabase={N}` DN. A replicated data database is never a leaf — it carries
`olcOverlay={N}syncprov` and `olcOverlay={N}accesslog` — and slapd refuses to
delete a non-leaf (`notAllowedOnNonLeaf`, `bconfig.c`'s `ce->ce_kids` branch).
The failure was logged and the finalizer released anyway, so the CR vanished and
the database stayed served, unmanaged, on every pod. Mechanism and evidence:
`docs/reconcile-loop-fixes.md`, same date.

**The decision above is unchanged.** Three points of it are now implemented as
written, and one is extended:

- *Children first.* The teardown deletes the database's whole `cn=config`
  subtree, deepest first. Deleting a child while keeping its parent would be a
  different act with a different rule (see `unwantedLogDBChildren`); here the
  parent is going away on explicit request, so leaving a child behind does not
  preserve it, it only makes the delete fail.
- *"Each pod's cn=config" includes the read-only StatefulSet.* An RO replica
  carries its own copy of the data database; `cn=config` is node-local (ADR-002).
- *The finalizer blocks, as this ADR always said it should.* A teardown that
  could not complete on some pod keeps the finalizer and retries; the error names
  the finalizer so an admin can force deletion. Previously it was removed
  unconditionally, which made a failed cleanup indistinguishable from a
  successful one.
- **Extension:** `Delete` also removes the database's own accesslog database
  (`cn=accesslog-<dbname>`), which did not exist when this ADR was written.
  ADR-019 gives each data database its own journal, so it has exactly one
  referent and the CR being finalized is it — the delete is authorised by state
  the operator owns, satisfying ADR-026 R2. The data database goes **first**, so
  its `olcAccessLogDB` reference dies with its referrer rather than dangling
  (ADR-026). Consistent with the "no destructive filesystem operations" stance
  above, the LMDB files under `/accesslog/<dbname>` are left on disk.

## Related

- ADR-004: Multi-resource CRD architecture — `SlapdDatabase` is one of the three CRDs.
- ADR-002: `cn=config` is node-local — why teardown is per pod, RO pods included.
- ADR-013: hot database add/remove is deferred — a `Delete` teardown still rolls
  the cluster, because `DATABASE_DIRS` shrinks with the CR.
- ADR-019: one accesslog DB per data DB — why the journal has exactly one
  referent and can be reaped with its database.
- ADR-026: R1 (re-resolve after any database delete) and R2 (never destroy
  shared state on foreign evidence) — both load-bearing for the teardown order.
- mariadb-operator `Database` CR — prior art for the cleanup policy pattern.
