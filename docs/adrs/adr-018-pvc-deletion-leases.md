# ADR-018: Co-located PVC access — RWO is per-node, and a pod object is a deletion lease

**Status:** Accepted
**Date:** 2026-08-23

## Context

Several operator components need *filesystem* access to a slapd pod's volumes
rather than LDAP access: `slapcat` for backup, `slapadd` for restore, and
plausibly future work (reindex, compaction, offline verification, migration
tooling). ADR-014 established how: a co-located one-shot Job that mounts the
target pod's `config-`/`data-`/`accesslog-` PVCs.

Two properties of Kubernetes storage govern that pattern. ADR-014 analysed the
first and missed the second.

### Fact 1 (ADR-014, correct and unchanged): RWO is per-node, not per-pod

`ReadWriteOnce` binds a volume to a single **node**; multiple pods on that node
may co-mount it. The single-pod restriction is the separate, opt-in
`ReadWriteOncePod` mode, which slaptain does not use. A Job scheduled onto the
same node as the target slapd pod can therefore mount that pod's PVCs while
slapd holds them. This is what makes the co-located Job pattern possible, and
nothing here invalidates it.

### Fact 2 (this ADR): a pod object is a PVC deletion lease

**A pod object that names a PVC blocks that PVC's deletion for as long as the
object exists, regardless of the pod's phase.** `Completed` and `Failed` pods
hold the lease exactly as firmly as `Running` ones. Only the existence of the
API object matters.

Upstream, in `pkg/controller/volume/pvcprotection/pvc_protection_controller.go`:

```go
func (c *Controller) podUsesPVCForDeletion(logger klog.Logger, pod *v1.Pod, pvc *v1.PersistentVolumeClaim) bool {
	if pod.Spec.NodeName != "" {
		for _, volume := range pod.Spec.Volumes {
			if volume.PersistentVolumeClaim != nil && volume.PersistentVolumeClaim.ClaimName == pvc.Name ||
				!podIsShutDown(pod) && volume.Ephemeral != nil && ... {
				return true
			}
		}
	}
	return false
}
```

Read the precedence: `(PVC != nil && ClaimName == pvc.Name) || (!podIsShutDown(pod)
&& Ephemeral != nil && ...)`. For a real PVC reference the only conditions are
*scheduled* (`NodeName != ""`) and *name match*. There is no phase test on that
branch.

Two things make this counter-intuitive, and both are worth stating so nobody
re-derives the wrong answer from memory:

- `podIsShutDown` guards only the **ephemeral-volume** branch, and it is not a
  terminality test. It is `DeletionTimestamp != nil &&
  DeletionGracePeriodSeconds == 0` — "kubelet is provably done" — and exists to
  break a pod-finalizer/PVC-finalizer deadlock cycle, not to release finished
  pods.
- The `IsPodTerminated` skip one expects *does* exist in that file, but in
  `podUsesPVCForUnusedSince` — a different feature (unused-since accounting).
  It does not apply to the deletion finalizer.

So `kubernetes.io/pvc-protection` stays on the PVC, the PVC sits in
`Terminating`, and the StatefulSet controller then refuses to (re)create any pod
whose PVCs are mid-deletion:

```
Error syncing StatefulSet, requeuing  err="[pvc config-slapd-1 is being deleted,
  pvc data-slapd-1 is being deleted, pvc accesslog-slapd-1 is being deleted]"
```

The pod never returns, on a widening backoff, for as long as the lease is held.
Note the shape of the coupling: the Job does not hold the StatefulSet. It holds
a PVC, and the PVC blocks the pod re-roll.

### How this was found, and demonstrated

A multi-site e2e run left a cluster wedged at 2/3 `Degraded`. In-place restore
had run against the shared cluster, completing one Job per pod; some minutes
later the ADR-012 case-2 spec deleted `slapd-1` together with its three PVCs.
The PVCs stayed `Terminating` indefinitely, the StatefulSet logged `FailedCreate`
as above, and the only object still referencing those PVCs was the **Completed**
restore Job pod for ordinal 1. Deleting that single pod released all three PVCs
within the same second; the StatefulSet provisioned fresh ones and the pod
resynced its DIT from peers.

Isolated reproduction, to rule out a stale-informer explanation: one fresh
namespace, one PVC, one Job that writes a file and exits 0. Deleting the PVC
while the `Completed` pod existed left it `Terminating` with `pvc-protection`
held for 80 s and counting; deleting the `Completed` pod released it
immediately.

### Why it matters

The only operation gated by this is **deletion of a slapd PVC** — but that is
ADR-012 case 2, a supported recovery procedure ("a pod lost its volumes; let
syncrepl refill it"). Namespace teardown is unaffected, because deleting a
namespace reaps the pods too.

Lease lifetimes as found in the pre-ADR implementation:

| Job | Pod object reaped by | Lease lifetime | PVCs pinned |
|---|---|---|---|
| restore `rw-i` / `ro-j` | `TTLSecondsAfterFinished: 3600` only | 1 h after completion | **every** pod's config/data/accesslog |
| on-demand `SlapdBackup` | GC with its owning CR | until the CR is deleted — indefinite | pod-0's config/data/accesslog |
| scheduled backup | GC with the CR, via retention | until `maxCount`/`maxAge` evicts — days | pod-0's config/data/accesslog |

Retaining a `SlapdBackup` CR is the entire point of taking a backup, so the
on-demand row is effectively permanent: a cluster that has ever backed up and
kept the record can never run case-2 recovery on pod-0. That is a silent
failure, whose only symptom is a StatefulSet event.

## Options considered

**Document the hazard, change no code.** Rejected. "Do not delete a PVC while a
backup record exists" makes a documented recovery procedure conditional on
invisible state, and the failure mode is a hang with no actionable message.

**Shorten `TTLSecondsAfterFinished`.** Rejected as *the* mechanism: it narrows a
window that should not exist, and leaves the lease lifetime coupled to a timer
rather than to the work being finished. Retained as a *backstop* (see R4).

**Stop mounting slapd PVCs at all** — network `ldapsearch` dump, or
VolumeSnapshot-based physical copy. Rejected here by reference: ADR-014 already
rejected the network dump on fidelity grounds and deferred the physical copy.
Named because it is the only *structural* escape from leases; if those trade-offs
ever change, this constraint disappears with them.

**Capture Job logs into the CR before reaping, to preserve diagnosability.**
Rejected. Log retention is not this operator's responsibility — anyone running
backups runs log aggregation (Loki, Graylog, Splunk, …), and that is the right
layer for Job output. Building a log-shipping path into the operator to work
around a PVC lifecycle constraint is the wrong trade, and would need `pods/log`
RBAC for no lasting benefit. Corollary accepted deliberately: the e2e suite does
**not** deploy logging infrastructure, so e2e diagnosis relies on CR status plus
tests that name the lease holder (R6).

**Operator-owned reaping.** Chosen.

## Decision

**The operator owns the reaping of every pod object it creates that mounts a
slapd PVC.** A co-located Job is not merely a transient co-mount; it is a lease
on the PVC's lifecycle, and the operator is responsible for ending it as soon as
the work is done.

Derived rules, binding on all present and future consumers of the co-located-Job
pattern:

- **R1 — Every mounting pod is a lease.** Any Job (or bare pod) the operator
  creates that mounts a slapd PVC holds that PVC's deletion open for the
  lifetime of the pod object. Treat creating one as taking a lock on PVC
  lifecycle, and account for its release explicitly.
- **R2 — Reap promptly on success *and* on failure.** No "retain the failed Job
  for inspection". Diagnosis of Job *content* belongs to the user's log stack.
  The operator does record the terminal outcome it can observe — the Job's
  failure condition/reason and the failed container's exit code — into the owning
  CR's status **before** reaping, since status is the operator's own contract.
  Failure is the case where the lease hurts most, not least: a failed restore
  holds the cluster at 0 replicas, so slapd is already down, PVC-level recovery
  is exactly what the admin reaches for, and the retained Job pods would be the
  only thing blocking it.
- **R3 — Reap from a persisted terminal state, never from the transition that
  detected it.** Both controllers create their Job on `Get` → `NotFound`, and
  these Jobs wipe data. Reaping in the same pass that observes completion risks
  a re-entrant pass recreating a destructive Job if the status write did not
  land. Reap in (or after) the branch guarded by an already-persisted terminal
  phase, and tolerate `NotFound` so the reap is idempotent under ADR-001
  double-reconcile.
- **R4 — `TTLSecondsAfterFinished` is a crash backstop, not the mechanism.**
  Keep it, short (order minutes), to bound the lease if the operator dies
  between the Job finishing and the reap. Never rely on it for correctness.
- **R5 — Co-location stays.** The lease is the price of filesystem access, not a
  reason to abandon Fact 1. Do not "fix" this by moving to
  `ReadWriteOncePod`, by dropping the affinity term, or by sidecars — see
  ADR-013 and ADR-014's rejected options.
- **R6 — Anything that deletes a slapd PVC must expect leases and name the
  holder.** Tooling and tests that wait on PVC deletion must, on timeout, report
  which pods still reference the PVC. A bare "timed out after 3 minutes" cost
  real debugging time in the incident above and is not an acceptable diagnostic.

## Consequences

- Restore Jobs are deleted once the cluster is back up, before `status.restore`
  is cleared (the restore ID is needed to select them). Backup Jobs are deleted
  once the backup is `Completed` and its object key is recorded, from the
  controller's existing terminal-phase early return — which satisfies R3 for
  free.
- No RBAC change: `jobs: create/get/list/watch/delete` is already granted.
- Successful Job logs are not retained. Accepted per R2; status carries path and
  size for backups, and the restore machine's status carries per-database
  progress.
- Failed Job logs are not retained either. This is the deliberate part of R2 and
  the one an operator without log aggregation will feel. The ADR-014 note that a
  human should "inspect/delete the failed Job" no longer applies and is
  superseded by R2's status capture.
- A cluster that has taken backups can once again run ADR-012 case-2 recovery on
  pod-0. That is the user-visible point of this ADR.
- e2e covers the invariant, not just the symptom: once a Job's work is done, no
  *terminated* pod references any slapd PVC. In-flight pods legitimately hold
  leases while they work, so restricting the assertion to Succeeded/Failed pods
  is both the correct reading of the rule and what makes the check safe to run
  alongside other specs' live Jobs.
  Note on where it lives: the obvious move — retarget the case-2 spec at pod-0,
  since pod-0 is what backup Jobs pin — was rejected. That spec deliberately
  picks a non-seed ordinal to stay orthogonal to ADR-012 seed semantics, and
  retargeting it would entangle two concerns. The invariant instead gets its own
  self-contained spec that takes a backup and asserts the reap, which needs no
  cross-file spec ordering to be deterministic. The case-2 spec keeps its
  ordinal and gains R6 diagnostics.
- `slctl` is a natural place to surface leases (an `inspect` check that flags
  pod objects referencing slapd PVCs and are not part of the StatefulSet), so
  R6's diagnosability is available interactively and not only in tests.

## Related

- ADR-001 — double reconciliation runs must be harmless; the idempotency
  requirement R3 leans on.
- ADR-012 — seed is one-shot; case 2 (a pod loses its PVCs and recovers via
  replication) is the procedure this constraint was silently breaking.
- ADR-013 — persistent storage is required, and the precedent for declining
  permanent management sidecars (R5).
- ADR-014 — S3 backup and restore; established the co-located Job pattern and
  Fact 1. Its Executor section's PVC analysis is *extended*, not reversed, by
  this ADR.
