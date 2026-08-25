# Bug analysis: `DATABASE_DIRS` env var triggers STS rolling restart on SlapdDatabase add

**Status:** Confirmed bug, fix design pending
**Discovered:** 2026-05-14, by the v0.0.14 ephemeral e2e fixture
**Severity:** Catastrophic on ephemeral storage (total data loss). Invisible-but-real on persistent storage.

This document is the analysis writeup for the design discussion. It is self-contained: a reader can come to it cold without knowing the conversation that produced it.

---

## TL;DR

The operator's `SlapdCluster` controller embeds `DATABASE_DIRS=<comma-separated>` as an env var on the slapd-init container. The env var lists the SlapdDatabase CRs in the namespace, and the init container `mkdir -p`s `/data/<name>` for each so slapd's back-mdb has somewhere to put its LMDB files.

When a `SlapdDatabase` CR is **added or removed** to a `Running` cluster, the operator recomputes `DATABASE_DIRS` and re-renders the STS spec. The pod template hash changes. The StatefulSet rolls every pod.

- **Persistent storage**: invisible. PVCs survive the restart; data is preserved.
- **Ephemeral storage (emptyDir)**: catastrophic. Every restarting pod loses everything; OrderedReady's "Ready" check doesn't include "data converged via syncrepl"; the rolling sequence happens faster than convergence; the cluster ends up empty across all pods.

The bug was masked for the lifetime of the project because the persistent fixture absorbed it silently. The v0.0.14 ephemeral fixture is what surfaced it.

---

## How v0.0.14's safeguards behaved (correctly!)

This bug is exactly the kind of failure ADR-012 was designed to make visible. The system reported the truth:

- `Status.SeedApplied = true` (correct — seed *was* applied once, on the pod that's now empty)
- Operator did **NOT** silently re-seed (per ADR-012's one-shot-latch contract; re-seeding would have given a fake-recovery illusion with just the 6 spec entries instead of any real user data)
- `Conditions[type=DataPresent].status = False, reason = DataMissing`, with a clear message pointing the operator to ADR-012

The condition correctly classified this as "data loss, requires human action" rather than "transient, will resolve". Without those guardrails, two failure modes would have hidden:

1. A re-seed would have produced a directory with only the spec.seed.entries — looking healthy on cursory inspection but in fact missing every user-added entry.
2. Without `DataPresent`, the directory-empty state would have been visible only through query failures and complaints from clients.

**The bug is real and the operator's diagnostic surface worked.**

---

## Reproduction

The cleanest way to trigger it deterministically:

```bash
# Deploy SlapdCluster only — no SlapdDatabase yet
helm upgrade --install slapd ./charts/slapd-cluster \
  --namespace slaptain-testing-ephemeral --create-namespace \
  -f tests/values.slapd-ephemeral.yaml

# Wait for the StatefulSet to reach Ready with empty DATABASE_DIRS=""
kubectl rollout status -n slaptain-testing-ephemeral statefulset/slapd

# Now apply the SlapdDatabase — this is the trigger
kubectl apply -n slaptain-testing-ephemeral -f tests/resources/example/database.yaml

# Watch the rolling restart:
kubectl get pods -n slaptain-testing-ephemeral -w
# All three RW pods restart in OrderedReady sequence.
# Each one starts with an empty /data emptyDir.
# The cluster ends up with zero LDAP entries.
```

Or in the unified e2e: `E2E_SKIP_PERSISTENT=1 ./tests/e2e.sh test <ctx>` against either single-site or multi-site. The ephemeral fixture's setup phase triggers it on every run.

---

## Observed timeline (one specific reproduction, t3e cluster, 2026-05-14)

| Time (UTC) | Event |
|---|---|
| 16:04:56 | Operator creates STS, all 4 pods scheduled. `DATABASE_DIRS=""` because no SlapdDatabase exists yet. |
| 16:04:54-16:05:00 | Operator tries to create the data DB on pods. Fails: `LDAP Result Code 80 "Other": olcDbDirectory: value #0: invalid path: No such file or directory`. (Slapd's back-mdb refuses to register a database whose backing directory doesn't exist.) |
| ~16:05 | Setup script applies SlapdDatabase CR. |
| Operator's SlapdCluster controller observes via Watch | Re-renders STS spec with `DATABASE_DIRS=example-db`. SSA patches the STS. Pod template hash changes. |
| 16:05:05 | slapd-0's `contextCSN` advances — seed applied to slapd-0. |
| 16:07:05, :34, :38 | Operator reconcile sees "replication user already exists" — DB is functional, syncrepl is starting. |
| 16:07:44 | slapd-1's `contextCSN` advances — replication has reached slapd-1. |
| **16:07:46** | StatefulSet starts rolling restart triggered by the template change. |
| 16:07:46-53 | All three RW pods restart with empty `emptyDir` volumes, sequentially per OrderedReady. |
| 16:07:52 | `DataPresent` condition flips to `False` reason `DataMissing`. |

The damning gap: between seed-applied (16:05:05) and rolling-restart-starts (16:07:46) is about 2m41s. In that window, the operator finished applying the seed and started writing replication state. But syncrepl on multi-master with the operator-managed configuration takes a few seconds-to-tens-of-seconds to fully drain initial state to all peers. The rolling restart began before that completed.

---

## Root cause walkthrough

### The dependency chain that creates the rolling restart

1. `SlapdCluster.reconcileStatefulSet` in `operator/internal/controller/slapdcluster_controller.go:454`
2. Calls `buildStatefulSetSpec(sc, false, databaseNames)`
3. `buildStatefulSetSpec` includes init env (around line 834):
   ```go
   {Name: "DATABASE_DIRS", Value: strings.Join(databaseNames, ",")},
   ```
4. `databaseNames` is computed by listing `SlapdDatabase` CRs in the namespace whose `spec.clusterRef` matches.
5. `r.Patch(ctx, sts, client.Apply, client.ForceOwnership, client.FieldOwner(fieldManager))` — SSA, which produces a diff when `databaseNames` changes.
6. The diff is in `spec.template.spec.initContainers[].env`. K8s computes a new pod template hash. StatefulSet controller rolls.

The init container then reads `DATABASE_DIRS` at `images/slapd-init/bootstrap.sh:45-54` and `mkdir -p /data/<dir>` for each name.

### Why the operator's SlapdCluster controller re-reconciles when a SlapdDatabase changes

From `docs/reconcile-loop-fixes.md`, the 2026-04-20 entry "SlapdDatabase controller missed externalPeers changes on SlapdCluster" added a cross-CRD watch:

```go
Watches(&SlapdDatabase{}, EnqueueRequestsFromMapFunc(...))
```

This is correct for the case it was solving (external peer wiring). But it has the side-effect that *every* SlapdDatabase create/update/delete fires a SlapdCluster reconcile — which, if `databaseNames` changed, will re-render the STS spec.

### Why OrderedReady + RollingUpdate doesn't save us on ephemeral

- StatefulSet sets `updateStrategy.type = RollingUpdate`, `podManagementPolicy: OrderedReady`.
- During rolling update, pods are restarted highest-ordinal first (e.g., slapd-2, then slapd-1, then slapd-0).
- Before proceeding to the next pod, the controller waits for the previous one to be **Ready** in the Kubernetes sense: containers running, readiness probes passing.
- **slapd's readiness ≠ data convergence.** slapd is "Ready" as soon as it's listening on its ports. A pod that just started with an empty data DB is fully Ready in seconds.
- Syncrepl needs many seconds-to-minutes to drain a peer's full DIT into a fresh empty pod, depending on data size and contextCSN bookkeeping.

So the rolling sequence:

1. slapd-2 killed → restart → empty `/data` → slapd starts → **Ready in ~2-5s** → STS controller proceeds.
2. slapd-1 killed → restart → empty `/data` → Ready in ~2-5s → proceed.
3. slapd-0 killed → restart → empty `/data` → Ready in ~2-5s → done.

By the time step 3 runs, the only pod that had the original data (slapd-0, which received the seed) is also being wiped. No peer has the data to syncrepl from. After all three restarts, every RW pod has an empty data DB. The data is gone.

### Why we don't see this on persistent storage

The same rolling restart still happens. But the persistent PVC survives across pod replacement. Init container on each restarted pod sees `$CONFIG_DIR/slapd.d/cn=config` and `$DATA_DIR/example-db` already populated. It skips bootstrap. slapd starts with the existing data DB intact. Replication resumes from the persisted contextCSN cookies. Invisible to operators.

---

## Why the test fixture exposed this

v0.0.14 added the **ephemeral fixture** specifically to test "pod restart with full data loss → recovery via replication" (the `dataloss_recovery_test` plus the `ephemeral-only` label system). The fixture is the same shape as the persistent one (3 RW + 1 RO with replication) but with `persistence.enabled: false`, so `/config`, `/data`, `/accesslog` are all `emptyDir`.

This was an intentional design choice (ADR-012, "Case 2: multi-pod loses-one-pod data → syncrepl recovers"). The dataloss_recovery test deliberately deletes one pod mid-suite to verify recovery. That single-pod scenario works correctly — replication recovers.

But the ephemeral fixture's **setup phase** also exposes the *DATABASE_DIRS-triggered rolling restart* bug, which is a different, pre-existing bug that no other test before v0.0.14 could trigger because no other fixture lost data on pod restart.

---

## Adjacent issues uncovered along the way

These are NOT the main bug but worth tracking, because they are symptoms or downstream effects:

### 1. False `PartiallyApplied` message when only replication is pending

In the post-data-loss state, the SlapdDatabase status reports:
```yaml
- type: Ready
  status: "False"
  reason: PartiallyApplied
  message: "Database applied to 4 pods, 0 failed or pending"
```

The phrase "0 failed or pending" contradicts "PartiallyApplied". Looking at `slapddatabase_controller.go` around the status-setting block:
- `pendingWork` becomes true via failed pods OR via `skipped` flag from `reconcileReplication`
- The status message template only counts `failedPods`, not "replication setup is pending"
- Message should distinguish "0 failed pods, but replication setup is incomplete"

### 2. `DataPresent` stays `NotSeeded` when data came from external replication (homelab consumer-only case)

The v0.0.14 `DataPresent` logic in `evaluateDataPresent` gates on `Status.SeedApplied`:
```go
if !sd.Status.SeedApplied {
    cond.Status = metav1.ConditionUnknown
    cond.Reason  = "NotSeeded"
    return cond
}
```

This is wrong for the consumer-only migration scenario where the SlapdDatabase has no `spec.seed.entries` — data comes from external syncrepl, so `SeedApplied` never flips. Even though the directory legitimately has data, `DataPresent` stays `Unknown`.

Fix: drop the `SeedApplied` gate entirely. `DataPresent` should be "is the root entry visible on any reachable pod, yes/no/unreachable" — independent of how the data got there.

### 3. `slctl inspect` "missing cn=accesslog" in consumer-only mode

Separate slctl false-positive. Consumer-only mode legitimately has no accesslog DB; slctl's check was mode-unaware.

**Resolved 2026-08-25** (ADR-019 Phase 6). `slctl inspect` now computes the
*expected* set of accesslog DBs from intent rather than assuming one per RW pod:
`SlapdCluster.NeedsAccesslog()` gates whether any log is expected at all, and
`SlapdDatabase.DeltaSyncEnabled()` selects which databases get one. A
consumer-only cluster, and a single-replica cluster with no external peers, now
expect none and pass.

---

## Possible fixes

Five options, with tradeoffs. Three of them are real candidates, two are documented for completeness.

### A. Test-side workaround: reverse the apply order

Apply `SlapdDatabase` + `SlapdSchema` *before* waiting for `SlapdCluster` Running. The operator's first STS reconcile sees `databaseNames = [example-db]` and creates the STS with the correct `DATABASE_DIRS` baked in. No subsequent template change, no rolling restart.

- **Effort:** ~5 lines in `tests/e2e.sh` swapping the order of `setup_slapd_clusters` / `wait_for_clusters_ready` / `setup_test_resources`.
- **Pro:** Tiny change. Unblocks CI today.
- **Pro:** Realistic — a production deployment via flux/helm would apply both CRs in one pass anyway.
- **Con:** Doesn't fix the underlying bug. Users adding a SlapdDatabase to a Running cluster (a totally legitimate operation) still face it.
- **Con:** Slight race risk: even with the order swap, the operator's SlapdCluster reconcile might fire before the SlapdDatabase CR is fully indexed. We'd need to verify operator behaviour under that race.

### B. Wrapper entrypoint on the slapd container, reading cn=config

Replace slapd image's ENTRYPOINT with a tiny statically-linked Go binary (or a busybox-based shell) that, before exec-ing slapd:

1. Reads `/config/slapd.d/cn=config/olcDatabase={*}*.ldif` to extract `olcDbDirectory` values.
2. `mkdir -p` each of them.
3. `exec /usr/sbin/slapd <original args>`.

The init container is no longer responsible for data directories — only for cn=config infrastructure (schemas, modules, TLS, admin password). The wrapper handles data dirs at slapd start time.

But there's a chicken-and-egg: the data DB entry in cn=config is added by the SlapdDatabase controller via LDAP, AFTER slapd is running. So at start-time, cn=config doesn't yet know about the data DB. The wrapper can't pre-create the directory before slapd starts.

**Two sub-options to resolve the chicken-and-egg:**

- **B1:** Wrapper re-runs the mkdir scan in a loop or on signal. When the SlapdDatabase controller adds an olcDatabase entry, it also sends a signal/touches a file/RPC to trigger the wrapper. Heavier.
- **B2:** Make slapd's back-mdb auto-create the directory. This requires either patching back-mdb (upstream change) or pre-creating the directory in the LDAP-add step of `reconcilePodDatabase`. The latter requires shell-in-pod access (see Option C).

Option B alone doesn't solve the problem cleanly.

### C. SlapdDatabase controller execs into slapd container to mkdir

Before the controller's `ldap_add olcDatabase=...` step (where it tells slapd "make me a database at /data/example-db"), it `kubectl exec`s `mkdir -p /data/example-db` in the slapd container.

Requires the slapd image to have a shell (or at least `mkdir`). That breaks the distroless property.

- **Effort:** Medium. Operator code change + slapd image change (debian-slim base instead of distroless, or chiseled busybox).
- **Pro:** Solves the problem at the right architectural layer (the controller that *wants* the directory creates it).
- **Pro:** No STS template churn ever.
- **Pro:** Adding a new SlapdDatabase to a running cluster becomes a true hot operation (no pod restart at all).
- **Con:** Loses distroless. Some security regression.
- **Con:** Operator needs RBAC for `pods/exec` in the slapd namespace.
- **Con:** `kubectl exec` is awkward when the container's PID 1 is slapd itself — needs a different entrypoint or a small "slaptools" sidecar.

### D. Operator computes `DATABASE_DIRS` at first-create-time only, never updates

Never patch DATABASE_DIRS after STS creation. Adding a new SlapdDatabase fails to materialize the data directory; operator surfaces this as a status condition "directory missing, requires manual pod restart to apply".

- **Pro:** Trivial code change.
- **Con:** Terrible UX. Adding a new database requires manual intervention to restart pods. And then ephemeral storage loses data anyway during that manual restart.
- **Decision:** Not a real candidate.

### E. ConfigMap-mounted file instead of env var

Mount a ConfigMap with `DATABASE_DIRS` content at a known path. The init container reads from file instead of env. ConfigMap updates don't restart pods.

But: the init container only runs at pod start. Updating the ConfigMap content doesn't fire init again. Adding a new SlapdDatabase post-bootstrap still leaves /data/newname uncreated until a pod restart.

- **Decision:** Doesn't actually solve the problem.

---

## My (recommended) reading

A is the right immediate fix to land. C is the right architectural fix to file and schedule. B is a partial solution that doesn't pull its weight.

But the **decision belongs to the user** — explicitly the question of whether to invest in the distroless-loss tradeoff in C, or live with A and add a status condition that says "if you add a SlapdDatabase to a Running cluster, the operator will trigger a rolling restart, which is safe on persistent storage but will lose data on ephemeral storage, please drain your data first."

---

## Open questions for the design discussion

1. **Test-side now, operator-side later (Option A first), or operator-side now (Option C)?**
2. **Is losing distroless on the slapd container acceptable for the operability win of Option C?** If yes: which base — `debian:trixie-slim`? `gcr.io/distroless/base-debian13-debug`? A chiseled busybox? Custom-built minimal image with just `mkdir`/`chown`/`exec`?
3. **Should there be an ADR-013 codifying the "data-dir creation contract"?** It would document: who creates `/data/<dbname>`, when, why, and what happens if it's missing.
4. **Should `DataPresent` lose its `SeedApplied` gate** (adjacent issue #2) so the consumer-only-from-replication case reports correctly? This is independent of the main bug but came up during diagnosis.
5. **Should the false `PartiallyApplied` message be fixed in the same PR** (adjacent issue #1) or as a separate trivial commit?
6. **Should we also fix `slctl inspect`'s naming-context check** (adjacent issue #3) for consumer-only mode? Independent of operator, but came up in the same investigation.
7. **What about the implicit contract that "Ready means data converged"?** This affects more than just the rolling-restart case — anywhere we rely on StatefulSet's pod-ready-then-proceed semantics, we have a latent issue with slapd readiness not reflecting data state. Worth a readiness probe that does a base-scope search on the suffix?

---

## Code references

- `operator/internal/controller/slapdcluster_controller.go`
  - `reconcileStatefulSet` (~line 454)
  - `buildStatefulSetSpec` and the `DATABASE_DIRS` env var (~line 834)
  - `adoptImmutableSTSFields` (~line 494) — relevant context: the operator is careful about immutable STS fields, but `DATABASE_DIRS` is in the mutable `template.spec.initContainers[].env` path
- `operator/internal/controller/slapddatabase_controller.go`
  - `reconcilePodDatabase` (~line 374) — does the LDAP-add for `olcDatabase`, where the missing-directory error fires
  - `applySeedData` (rewritten in v0.0.14)
  - `evaluateDataPresent` (~line 1191) — the DataPresent condition I want to revisit per #2 above
  - the status-setting block where "PartiallyApplied" message is generated — per adjacent issue #1
- `images/slapd-init/bootstrap.sh`
  - lines 45-54 — `DATABASE_DIRS` parsing and `mkdir -p`
- `tests/e2e.sh`
  - `setup_slapd_clusters`, `wait_for_clusters_ready`, `setup_test_resources` — the order to swap for Option A
- `tests/values.slapd-ephemeral.yaml`
  - the fixture that surfaced this
- `docs/adrs/adr-012-seed-and-lifecycle.md`
  - the framework that made the bug visible without silently re-recovering
- `docs/reconcile-loop-fixes.md` 2026-04-20 entry
  - the SlapdDatabase → SlapdCluster watch that's part of the dependency chain

---

## Evidence artifacts

The slctl debug-dump from the failed run is at `slctl-debug-slapd-20260514-182729/` in the repo root (gitignored). Files of interest:
- `slapdcluster.yaml`, `pod-slapd-*.yaml` — current state showing restart times
- `events.txt` — STS create/delete events
- `operator-logs.txt` — operator activity, including the "olcDbDirectory invalid path" errors and the seed/replication setup race
- `contextcsn-slapd-readonly-0.txt` — shows the writes that happened (16:05:05 from serverID=1 i.e. slapd-0, 16:07:44 from serverID=2 i.e. slapd-1)
- `rootdse-slapd-*.txt` — confirms the data DB is configured (naming contexts include `dc=example,dc=org`) but it's empty (no actual entries)
