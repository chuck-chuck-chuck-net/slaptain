# ADR-001: Double Reconciliation Runs Are Harmless

**Status:** Accepted
**Date:** 2026-03-01

## Context

During initial deployment of a 3-replica SlapdCluster, the operator logs showed pairs
of reconcile runs at the same timestamp, each with a distinct `reconcileID`:

```
2026-03-01T07:06:08Z  pod-0 not yet ready  reconcileID=64e03ad0
2026-03-01T07:06:08Z  pod-0 not yet ready  reconcileID=88f03bdf
2026-03-01T07:06:18Z  pod-0 not yet ready  reconcileID=cc4ada47
2026-03-01T07:06:18Z  pod-0 not yet ready  reconcileID=f7d63f72
```

A similar pattern in Redis Sentinel operators is known to be
harmful: the second run issued commands to Sentinel mid-election, corrupting its internal
state. We needed to confirm that the same problem does not apply here.

## Root Cause of the Double Runs

Two independent sources populate the controller-runtime workqueue:

1. **`RequeueAfter: 10s`** — returned when pod-0 is not yet ready; schedules the item
   via `queue.AddAfter(req, 10s)`.

2. **`Owns()` watch events** — `Owns(&appsv1.StatefulSet{})` registers an informer on
   owned StatefulSets. During pod startup the StatefulSet status is updated on every pod
   transition (scheduled → init running → init complete → ready), producing multiple
   events per second.

The workqueue deduplicates by key, but only when the item is **not currently being
processed**. When an `Owns` event arrives while a reconcile is already running, it is
placed in an internal dirty set and re-queued immediately after the running reconcile
finishes. This produces two sequential runs within the same wall-clock second.

## Why the Runs Are Harmless

**Sequential, not concurrent.** `worker count: 1` (the controller-runtime default) means
no two goroutines are ever inside `Reconcile` simultaneously. The "double" is run A
finishing, then run B starting ~5 ms later. There is no shared mutable state being
accessed concurrently.

**Each reconcile step is idempotent or guarded:**

| Step | Guard |
|---|---|
| `reconcileSecret` | Existence check: if `<name>-passwords` exists → return immediately |
| `reconcileHeadlessService` / `reconcileClusterIPService` | `CreateOrUpdate` — no-diff update is a no-op |
| `reconcileStatefulSet` | `CreateOrUpdate` with deterministic spec — both runs produce identical output |
| `reconcileBootstrap` | (1) `if sc.Status.BootstrapComplete { return nil }` at top; (2) `ldapEntryExists` check before any LDAP ADD |

The bootstrap double-run was observed and confirmed harmless in the logs:

```
bootstrap complete          ← run A: added root + admin + replication entries
root entry already exists   ← run B: ldapEntryExists returned true, no ADDs issued
```

**No ongoing in-app reconciliation to interrupt.** The operator's only runtime
interaction with slapd is the one-time LDAP bootstrap (base entry ADDs). It does not
issue topology-change commands (e.g. `ldapmodify` on `cn=config`) to a running slapd
instance. Delta-syncrepl is self-managing after init-container startup; there is no
operator-driven replication state machine that a second reconcile could corrupt.

## Decision

No changes required. The double reconciliation runs are an expected consequence of
combining periodic `RequeueAfter` polling with event-driven `Owns` watches, and the
reconciler is structured to be safe under this pattern.

If a future reconcile step needs to issue commands to a live slapd instance (e.g. Phase 3
topology reconfiguration via `cn=config`), that step must either:
- carry its own idempotency guard (check current config before modifying), or
- use a finalizer / status field to record that the command was issued, so a second run
  can detect and skip it.

## Consequences

- The periodic requeue interval (currently 10 s) may be increased if the extra reconcile
  load becomes a concern at scale, but there is no correctness reason to do so now.
- Any future developer adding a reconcile step that talks to a live slapd instance must
  read this ADR and ensure the new step satisfies the idempotency requirement above.
