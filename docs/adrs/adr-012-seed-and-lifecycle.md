# ADR-012: Seed is one-shot; cluster wipe is a Kubernetes resource lifecycle operation

**Status:** Accepted
**Date:** 2026-05-14

## Context

Two related operator behaviours grew up independently in early development and
turned out to be conceptually wrong once we ran them in anger:

1. **`spec.ldap.forceRebootstrap=true`** instructed the init container to
   `rm -rf` its mounted volumes on next pod start, after which the operator would
   re-seed the database tree. Documented as a "destructive but supported" recovery
   knob. In practice it required a six-step manual ritual (patch flag on, kubectl
   rollout restart, patch flag off, patch `status.bootstrapComplete=false`, patch
   SlapdDatabase `status.seedApplied=false`) and any skipped step left the cluster
   in a footgun state — most loudly, leaving the flag on meant every subsequent
   pod restart silently wiped data.

2. **`verifySeedExists`** ran on every reconcile after `Status.SeedApplied=true`:
   it connected to the first reachable RW pod, base-scoped the first seed entry,
   and if the entry was missing, set `needsSeed=true` and re-applied. This was
   the operator's "self-healing" response to the `forceRebootstrap` lifecycle
   (where data was destroyed but the latch had to be manually reset).

Both behaviours collapsed the boundary between "operator-owned declarative state"
(cn=config: ACLs, schemas, syncrepl stanzas, the data DB itself) and **user data**
(the entries the directory has accumulated since first bootstrap). They treated
seed entries as the latter — content the operator would re-create on demand —
when in reality the seed is the *former*'s one-time initial-conditions sketch,
not an ongoing source of truth.

## What went wrong

The `verifySeedExists` self-heal path produced two concrete failures we observed
in the 2026-05-13 multi-site e2e run:

1. **Multi-pod seed write race.** Initial reconcile after StatefulSet rollout:
   pod-0 and pod-1 were briefly unreachable while pod-2 was already accepting
   connections. `applySeedData` fell back to pod-2 and wrote all six seed entries
   there. Next reconcile (3s later), pod-1 came up; `verifySeedExists` checked
   pod-1, saw nothing (replication hadn't propagated yet), returned false; seed
   re-applied on pod-1. Next reconcile, same dance on pod-0. Three independent
   same-DN adds with three different `olcServerID`s created a CSN-conflict storm
   in the multi-master mesh. Entries 4-6 of the seed (ou=Mail, ou=ServiceAccounts,
   the readonly service account) lost the resolution race and went missing — the
   operator reported `seedApplied=true` and `Phase=Running` while the directory
   was silently incomplete.

2. **False recovery masking data loss.** In a hypothetical scenario where a
   single-pod cluster lost its PVC, `verifySeedExists` would have detected the
   empty directory and re-applied the seed entries. The cluster would have
   reported `Ready=True` and `seedApplied=true` again, while in fact thousands of
   user entries accumulated over the cluster's lifetime were gone. Re-seeding
   gives the illusion of recovery while real data is lost — strictly worse than
   failing loudly.

## Decision

### Seed is one-shot per cluster lifetime

`Status.SeedApplied` is a one-way latch. Once true, the operator never
re-evaluates seed state. The single seed application:

- Targets pod-0 deterministically. No fall-back to higher ordinals; if pod-0 is
  unreachable, retry next reconcile. This eliminates the multi-pod write race.
- Verifies each entry's persistence with a base-scope search on the same
  connection immediately after the add. Only on a complete, verified run does
  the caller flip `SeedApplied=true`.
- Operates idempotently on a per-entry basis: an `EntryAlreadyExists` reply from
  a partial earlier attempt is treated as "fine, verify it" rather than "error."

For databases participating in multi-master replication, replication propagates
the seed from pod-0 to peers — no operator intervention needed and no second
write path.

### Data after seed is not the operator's concern

The seed is the *initial conditions* the user provides for their directory.
After first apply, the actual contents of the directory are the users' data
(potentially millions of entries) which the operator never knew about and has
no business curating. The operator does **not**:

- Re-check seed entries' presence after `SeedApplied=true`.
- Re-apply seed entries if they go missing.
- Treat "data missing on this pod" as a trigger for action.

A separate informational `DataPresent` status condition on SlapdDatabase exists
purely as an observability signal (alert hook for ops teams). It reports whether
the suffix's root entry is visible on at least one reachable pod. It is **never**
read by the reconciler — it does not drive operator behaviour.

### Cluster wipe is a Kubernetes resource lifecycle operation

The supported way to discard a slaptain cluster's data and start fresh is:

```bash
kubectl delete slapdcluster <name> -n <ns>
kubectl delete pvc -n <ns> -l app.kubernetes.io/instance=<name>
# redeploy via helm / flux / kubectl apply
```

The cascade-delete on the CR removes the StatefulSet, Services, and Secrets via
ownerReferences. The PVC label-selector purge is necessary because StatefulSet
PVCs default to `persistentVolumeClaimRetentionPolicy.whenDeleted=Retain` (and
we don't override this — losing data on accidental STS deletion is a much worse
default than surviving it). On redeploy, the operator recreates everything from
spec, the init container re-bootstraps cn=config, and the seed runs again
because `SeedApplied` lived on the CR's status and didn't survive deletion.

### Per-pod data recovery is replication's job

For multi-pod clusters that lose a single pod's data (node failure, PVC delete,
manual intervention), the recovery path is OpenLDAP syncrepl: the empty pod
rejoins the mesh, the init container bootstraps cn=config, the SlapdDatabase
controller (re-)creates the data DB on cn=config, and syncrepl pulls the entire
DIT from peers. No operator-driven re-seed.

For single-pod clusters that lose data, there is no peer to recover from. The
operator's correct response is **inaction with surfaced observability** — the
`DataPresent=False` condition fires, the human investigates, restores from
backup or deliberately wipes and redeploys.

## Options considered

### A. Keep `forceRebootstrap` for "fast in-place wipe"

Rejected. The six-step manual procedure (with one footgun: forgetting to flip
the flag back wipes on every restart) was strictly worse than the
delete-CR-and-PVC path. No real use case survived scrutiny: static PVCs can be
wiped with `kubectl exec -- rm -rf` if needed; LoadBalancer IPs persist across
recreate when pinned via `loadBalancerIP`; the speed difference is seconds.

### B. Re-evaluate seed on every reconcile (status quo before this ADR)

Rejected. Conflates "operator-owned state" with "user data." Cannot
distinguish replication-warming-up from PVC-reset from intentional-user-delete.
Causes the multi-pod write race documented above. Masks real data loss with
fake recovery.

### C. Re-evaluate seed only on forceRebootstrap

Rejected. With (A) rejected, this becomes "never re-evaluate," which is what
we picked.

### D. Add a `DataPresent` condition that *triggers* re-seed

Rejected. The condition is valuable as observation but disastrous as an action
trigger — it would re-introduce false recovery in the single-pod data-loss case
and reduce the operator's correctness to "did our racy re-seed beat the user's
real data?" Observation only.

### E. Forbid all wipe operations; require fresh namespace for fresh cluster

Rejected. Wipe-and-redeploy is a real operational need (test environments,
disaster recovery from corruption). The k8s resource lifecycle handles it
cleanly with two existing commands.

## Consequences

**Removed:**
- `Spec.LDAP.ForceRebootstrap` field
- `FORCE_REBOOTSTRAP` env var plumbing in operator and init container
- `verifySeedExists` function and its call site
- Multi-step forceRebootstrap recovery procedure from ONBOARDING.md and
  BOOTSTRAP.md

**Added:**
- Deterministic pod-0 seed target with per-entry post-add verification
- Informational `DataPresent` condition on SlapdDatabase status
- "Wipe a cluster" recipe in ONBOARDING.md and BOOTSTRAP.md

**Behavioural changes:**
- A SlapdDatabase whose seed apply transiently fails will retry indefinitely on
  pod-0 until success; this is stricter than the old fall-back-to-pod-N
  behaviour but eliminates the multi-pod conflict storm.
- A SlapdCluster that loses data after initial seed will show
  `DataPresent=False` on its SlapdDatabase but will not self-heal. Human
  intervention (backup restore or deliberate wipe-and-redeploy) is required.
- Existing CRs with `spec.ldap.forceRebootstrap` set (including `:false`) will
  be rejected by strict-field validation on the new CRD and must be updated.

**E2E coverage:**
- Dual-fixture e2e suite (persistent + ephemeral SlapdCluster) ensures the
  pod-restart and data-loss recovery paths are exercised on every run. The
  ephemeral fixture uses `persistence.enabled=false` (emptyDir for config,
  data, accesslog) to emulate "pod restart with full data loss" without
  needing destructive PVC manipulation in the test orchestration.

## Related

- ADR-002: cn=config is node-local — established the "operator-owned declarative
  state" boundary that this ADR refines for seed data
- ADR-004: multi-resource CRD architecture — moved seed and Status.SeedApplied
  from SlapdCluster to SlapdDatabase
- ADR-005: SlapdDatabase cleanup policy — `Retain` (default) vs `Delete`
  semantics align with this ADR's wipe-via-CR-and-PVC-delete model
- `docs/reconcile-loop-fixes.md` 2026-04-19 "Seed data lost on rolling restart"
  entry — the original fix that this ADR supersedes, amended in place with a
  postscript explaining why
