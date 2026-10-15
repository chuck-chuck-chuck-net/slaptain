# ADR-013: Defer hot SlapdDatabase add/remove; require persistent storage

**Status:** Accepted
**Date:** 2026-05-14

## Context

Adding or removing a `SlapdDatabase` CR on a Running cluster currently triggers
a StatefulSet rolling restart of every slapd pod. The mechanism:

1. `SlapdCluster.reconcileStatefulSet` lists `SlapdDatabase` CRs in the
   namespace and embeds the result as `DATABASE_DIRS=<comma-separated>` in the
   slapd-init container's env block.
2. The init container `mkdir -p /data/<name>` for each entry, because slapd's
   back-mdb refuses to register an `olcDatabase` whose `olcDbDirectory` does
   not exist.
3. Any change to the SlapdDatabase set re-renders the STS pod template. The
   template hash changes. The STS controller rolls every pod.

Slapd itself does **not** require a restart to add or remove a database — the
operation is fully live over the wire protocol (`ldap-add` against `cn=config`).
The rolling restart is purely an artefact of our orchestration: we need a
directory on the pod filesystem before the live `ldap-add` will succeed, and our
only mechanism for creating it today is the init container, which runs at pod
start.

The behaviour was discovered on 2026-05-14 by the v0.0.14 ephemeral e2e fixture,
where the rolling restart on emptyDir storage caused total data loss. See
`docs/BUG-ANALYSIS-database-dirs-rolling-restart.md` for the full investigation.
On persistent storage the rolling restart still happens but is data-safe — the
PVCs survive the pod replacement and slapd resumes from on-disk state.

## What this ADR decides

Two related decisions:

1. **Persistent storage is required.** The `spec.persistence.enabled` field is
   removed from the v1alpha1 CRD entirely. The operator always provisions PVCs
   via the StatefulSet's volumeClaimTemplates. CRs that still carry the field
   are rejected at admission as an unknown field.
2. **The rolling restart on SlapdDatabase add/remove is accepted as-is.** We
   will not invest in eliminating it. Persistent storage makes it a UX wart
   (an unnecessary restart) rather than a data-correctness bug.

A separate but related question — should pod readiness reflect data convergence
rather than just "slapd is listening on a port" — is deferred for the same
symmetric reason (see below).

## Options considered for eliminating the rolling restart

### C' — bake a helper binary into the slapd image; operator `kubectl exec`s it

Ship a small statically-linked Go binary (`slaptools mkdir /data/<name>`) in
the slapd image. When the SlapdDatabase controller is about to issue
`ldap-add olcDatabase=...`, it `kubectl exec`s the helper to pre-create the
directory.

- **Pro:** Synchronous; no race. STS pod template never churns.
- **Pro:** Small change; one binary, no new container.
- **Con:** Operator needs `pods/exec` RBAC. Not a meaningful security
  regression (operator already has cluster-wide RBAC on Secrets/STS/Services
  and a network channel to slapd's `cn=config`), but it is a new verb.
- **Con:** Slapd image grows by one binary. Distroless-in-spirit preserved
  (still no shell), but the image is no longer purely "slapd and its
  dependencies".

### G — management sidecar with an authenticated mkdir API

Add a distroless/static sidecar to every slapd pod, sharing the `/data`
volume. The sidecar exposes an authenticated HTTPS endpoint (reusing the
existing TLS Secret for mTLS). The SlapdDatabase controller calls
`POST /mkdir { path: /data/<name> }` synchronously before `ldap-add`.

- **Pro:** Synchronous; no race.
- **Pro:** Slapd image unchanged.
- **Pro:** Sidecar becomes a natural extension point for future in-pod
  operations (cleanup, diagnostics, debug dumps).
- **Con:** One extra container per pod, permanently. Resource overhead, even
  if small.
- **Con:** New API surface to design and maintain (auth, protocol, error
  handling, observability).
- **Con:** Coupling between operator and sidecar must stay in sync across
  upgrades.

### F — ConfigMap-mounted database list with a reconciler sidecar

The SlapdCluster controller writes the database list to a ConfigMap (not the
STS pod template). A sidecar in every pod watches the ConfigMap mount and
reconciles directories. The SlapdDatabase controller issues `ldap-add` and
relies on its existing retry loop to bridge the ConfigMap propagation lag.

- **Pro:** Fully declarative; no operator-side RPC; no `pods/exec` RBAC.
- **Pro:** Slapd image unchanged.
- **Con:** Asynchronous. Propagation lag is ~60s (kubelet ConfigMap sync). In
  that window, `ldap-add` fails with `LDAP code 80: olcDbDirectory invalid
  path` — the exact error we already see during bootstrap, recoverable via
  the existing retry loop but produces transient status flap.
- **Con:** Same sidecar resource overhead as G, plus a less crisp failure
  semantics.

### Cost summary

| | C' | G | F |
|---|---|---|---|
| New code | ~100 LoC helper + ~200 LoC operator exec wiring | ~300 LoC sidecar (HTTPS, mTLS, mkdir handler) + ~200 LoC operator client | ~150 LoC sidecar reconciler + ~100 LoC operator ConfigMap writer |
| Image changes | slapd +1 binary | new sidecar image | new sidecar image |
| RBAC | + `pods/exec` | none | none |
| Pod overhead | none | +1 container | +1 container |
| Synchronous? | yes | yes | no (~60s race) |
| Ongoing maintenance | low | medium (auth, protocol drift) | low-medium |

All three options cost real engineering effort. None is a one-line fix.

## Why we decline to ship any of them now

Two arguments, both pointing the same direction:

1. **Frequency.** Adding or removing a SlapdDatabase on a Running cluster is
   not a routine production operation. LDAP data models are designed up front
   and provisioned once. The hot-add/hot-remove pattern is primarily a
   dev/test convenience. We're being asked to invest persistent engineering
   complexity (a new container, a new API, or a new RBAC verb) to preserve a
   UX property that is exercised maybe once per cluster lifetime.

2. **Severity on persistent storage.** With persistence now mandatory
   (decision 1 above; the spec field is gone), the rolling restart is
   data-safe. It is a UX wart — an unexpected restart when the user
   applies a CR — but not a correctness bug. Production users see a
   brief endpoint flap during the rolling update, not data loss.

The architectural fix is not wrong. It is correctly identified, designed, and
costed. We are deferring it because the value/cost ratio does not justify the
investment today. Reconsider if:

- We see real-world reports of users hitting the rolling restart and being
  surprised or hurt by it.
- The operator gains a sidecar for other reasons (debug ops, custom metrics
  collection, log shipping), at which point adding mkdir to its responsibility
  is nearly free.

## Data-convergence readiness probe — same decision, same reasoning

A related question surfaced during the discussion: should pod readiness reflect
"slapd has converged data" rather than "slapd is listening on a port"? Today
the readiness check is effectively "port open", which means a freshly-restarted
empty pod is considered Ready before syncrepl has filled its database. This is
what made the rolling-restart bug catastrophic on ephemeral storage — the STS
controller proceeded to the next pod before the previous one had caught up.

A real data-convergence readiness probe would solve this class of bug — not
just the DATABASE_DIRS trigger but any future STS pod template churn that
causes pods to restart faster than syncrepl can converge.

The standard chicken-and-egg objection (readiness gates pod membership in
Services; convergence requires Service reachability) is addressable: set
`publishNotReadyAddresses: true` on the headless service so peer-to-peer DNS
ignores readiness state, while the client-facing ClusterIP service continues
to gate on readiness. The operator already talks to pods via headless DNS for
all per-pod operations (cn=config is node-local), so this works.

The real obstacles are:

- **Definition of "converged".** What does the probe actually check? Base-scope
  on the suffix? `contextCSN` within some delta of the cluster max? Per-DB
  entry count above a threshold? Each option has corner cases.
- **Implementation surface.** kubelet probe options are HTTPGet, TCPSocket,
  Exec. Slapd doesn't speak HTTP, TCPSocket is what we have, Exec requires a
  binary in the distroless image — the same architectural cost as Option C'.

The symmetric argument applies: with persistent storage required, the class of
bug the convergence probe protects against is "UX wart during rolling updates"
(brief stale-or-empty reads), not "data correctness". Real-world frequency of
STS template churn is low. Defer.

If we ever pick this up, it should be its own ADR — the design surface (probe
definition, edge cases during initial bootstrap, interaction with consumer-only
replication) is non-trivial.

## Consequences

### Required follow-up

- **CRD field removal.** Drop `spec.persistence.enabled` from the v1alpha1
  CRD entirely. Existing CRs that carry the field will be rejected at
  admission as an unknown field (the same kind of v1alpha1 breakage already
  precedented by the `forceRebootstrap` removal under ADR-012). The "in-memory
  dev cluster" use case is well-served by k3s + local-path-provisioner —
  we don't need to support an unsafe path to serve it.

- **e2e suite:** drop the ephemeral fixture (`tests/values.slapd-ephemeral.yaml`,
  `ephemeral-only` label, dual-fixture orchestration in `tests/e2e.sh`).
  Replace `dataloss_recovery_test.go` with a `kubectl delete pod/<name>
  pvc/data-<name>` test against the persistent fixture. This more accurately
  simulates the real failure mode (node disk loss, accidental PVC deletion)
  than ephemeral-everywhere.

- **Unified `tests/e2e.sh`** stays. Its consolidation of single-site and
  multi-site logic is independently valuable and survives the descope.

- **Documentation:** update CLAUDE.md's "Test Fixture" section, BOOTSTRAP.md,
  and `docs/MIGRATION-PLAN.md` if they reference ephemeral testing.

### Behavioural changes for users

- Adding or removing a `SlapdDatabase` CR on a Running cluster triggers a
  rolling restart of all slapd pods. This is documented as expected
  behaviour.
- `spec.persistence.enabled` is gone from the v1alpha1 CRD. CRs still
  carrying it are rejected at admission.

## Related

- ADR-004 — multi-resource CRD architecture (SlapdCluster / SlapdDatabase /
  SlapdSchema).
- ADR-012 — seed is one-shot; cluster wipe is a Kubernetes resource lifecycle
  operation. Case 2 ("multi-pod: lose one pod's data, syncrepl recovers")
  retains its meaning but is now triggered by `kubectl delete pod + pvc` on
  persistent storage, not by pod restart on ephemeral.
- `docs/BUG-ANALYSIS-database-dirs-rolling-restart.md` — the detailed
  investigation that produced this ADR.

## Amendment (2026-09-28): `DATABASE_DIRS` is the *accepted* pod-template roll — everything else is a defect

This ADR accepted one rolling restart, for one reason: `DATABASE_DIRS` must be a
pod-template env var because only the init container can create a directory on
the pod filesystem, and the alternatives cost either the distroless property
(option C') or a permanent sidecar (option G).

That acceptance was read more broadly than it was written. A second pod-template
value — `LDAP_REPLICAS` — carried `spec.Replicas` into the init container's env,
so **every scale-up rolled the entire fleet**, and it went unremarked for months
in a repo whose own ADR says pod-template churn causes rolls. Its value had not
been read since ADR-017; see the 2026-09-28 entry in
`docs/reconcile-loop-fixes.md`.

**The rule this ADR did not state, and now does:** `DATABASE_DIRS` is the *only*
sanctioned pod-template roll trigger. Any other spec field that reaches the pod
template and can change during a cluster's life is a defect, not a wart — it
pays the price of option C' without buying its capability. In particular,
nothing that varies with `spec.Replicas` may enter the pod template: scaling is
a StatefulSet-level operation and must stay one.

The cost is not local, which is why the distinction matters. Under ADR-016 every
replaced pod invalidates the syncrepl addresses each remote site holds for it
(130-268 s measured). A mesh member that rolls four pods to add one replica
hands every peer four rediscovery windows for an operation that should have
produced exactly one new pod.

Guarded twice: `internal/controller/statefulset_replica_churn_test.go` compares
pod templates across replica counts above the 1 ↔ 2 boundary — where
`NeedsAccesslogVolume()` legitimately flips and the roll is correct — and
`tests/e2e/scale_topology_test.go` asserts on a live cluster that a scale in
either direction leaves the StatefulSet revision and every surviving pod's UID
untouched while `olcSyncRepl` is rewritten on each of them.

---

## Amendment, 2026-09-28 (second): the last sanctioned trigger goes too — option H

The amendment above named `DATABASE_DIRS` the *only* sanctioned pod-template
roll trigger. This one retires that exemption. The count of sanctioned triggers
becomes zero: **no spec field that can change during a cluster's life may reach
the pod template.**

### What changed since the deferral

Nothing about the frequency argument — adding a database on a Running cluster is
still not a routine production operation. What changed is the cost side, which
this ADR costed carefully in May and which is now wrong.

1. **ADR-018 (2026-08-23) gave the operator a co-located Job pattern**, three
   months after this ADR was written. `backup_job.go:55` mounts a *running*
   pod's ReadWriteOnce config and data PVCs, via required PodAffinity to that
   pod, while slapd holds them open. That is exactly the capability the `mkdir`
   needs, already built and already e2e-green. This ADR's reconsider trigger was
   "if the operator gains a sidecar for other reasons, mkdir is nearly free" —
   it gained something better.
2. **The mesh (ADR-028) multiplied the blast radius.** A database add rolls
   every pod at *every* site, and under ADR-016 each replaced pod costs every
   remote site a 130-268 s address-rediscovery window.
3. **The fleet-roll class is now the exception rather than the norm.** With
   `LDAP_REPLICAS` removed, this is the last accidental churn source; the
   remaining ones (`idlExponent`, an image change) are deliberate recreates.

### The problem is two problems

Separating them is the whole of the design, because one of them is free:

1. **Template churn.** The directory list rides as an env var on the init
   container (`slapdcluster_controller.go:962`). Any change re-renders the
   template. This fires on **add and remove alike**.
2. **The directory is absent on already-running pods.** Only on **add** —
   back-mdb will not register an `olcDatabase` whose `olcDbDirectory` does not
   exist. On remove there is nothing to do: an orphaned directory is disk, not a
   defect.

So fixing (1) alone removes the roll for every database *delete* — including the
one `tests/e2e/cleanup_policy_test.go` pays on each run.

### Option H — a per-pod mkdir Job (unlocked by ADR-018)

Before adding the database to a pod's `cn=config`, the SlapdDatabase controller
runs a short Job pinned to that pod's node, mounting that pod's data and
accesslog PVCs read-write, doing `mkdir -p`, and exiting. Image: `slapd-init` —
already present in every deployment, already has a shell, already runs as UID
1024, so the directory is created by the identity that will open the LMDB
environment and no `chown` is needed.

Against this ADR's own cost table:

| | C' | G | F | **H** |
|---|---|---|---|---|
| New code | ~300 LoC | ~500 LoC | ~250 LoC | **~120 LoC, cloned from `backup_job.go`** |
| Image changes | slapd +1 binary | new image | new image | **none** |
| RBAC | +`pods/exec` | none | none | **none — Jobs already** |
| Pod overhead | none | +1 container | +1 container | **none (transient)** |
| Synchronous? | yes | yes | no (~60 s race) | **yes — wait for completion** |

It dominates all three options this ADR costed. C', G and F are hereby rejected
rather than deferred.

**Honest costs.** A Job pod naming a PVC is an ADR-018 deletion lease and must
be reaped, including when it never schedules — a Pending Job on a cordoned or
full node holds the lease just as a finished one does, so this needs a timeout
and a legible condition, not an unbounded wait. N pods means N Jobs; serial is
fine, a database add is not latency-sensitive.

**Two reaping rules, not one, learned from the first live run.** The Job's name
is a hash of (pod, path set), which is what makes it idempotent — but it also
means the reap is keyed on the same hash. A path set that changes between
creating a Job and coming back to observe it (a second database applied moments
later, a scale across the 1↔2 accesslog boundary, an operator restart mid-flight)
leaves a Job that nothing will ever look up again, and only
`TTLSecondsAfterFinished` removes it — which ADR-018 R4 reserves for crashes.
Measured: a Job from the demo's one-database standalone stage outlived three
database adds and went at 5m03s, the TTL. So each pass first reaps this pod's
Jobs from any *superseded* path set, regardless of their state — a Job for a
path set that no longer applies has no reason to run, and a Pending one holds
the lease while never reaching a terminal state to be swept from.

### And the other half: the list leaves the pod template

The list moves to a ConfigMap, mounted as a file that the init container reads
instead of the env var. This is option F's first half without its sidecar — and
F's only real objection disappears with it. F was asynchronous because a sidecar
had to *act* on the ConfigMap within the `ldap-add` retry window; here nothing
urgent reads the file. Running pods get their directories from the Job; the file
is consulted only at a pod's next start, where a propagation lag of a minute is
invisible.

The radical version — drop the list entirely, since a fresh pod has an empty
config PVC and therefore no registered databases needing directories at boot —
is **rejected**. It is probably true, but it makes pod startup depend on
reasoning about divergent config/data PVCs (ADR-012 case 2), and the failure
mode when that reasoning is wrong is a pod that will not start.

### One latent defect this subsumes

`listDatabaseNames` (`slapdcluster_controller.go:1186`) collects `db.Name`, but
the directory a database actually uses is `spec.dataDirectory` when set, falling
back to the CR name (`slapddatabase_controller.go:863`). A CR that sets
`dataDirectory` therefore has the wrong directory created for it today. The
replacement must key on the **effective** directory name, in both the ConfigMap
and the Job.

### Consequences

- `DATABASE_DIRS` as a pod-template env var is removed. The pod template becomes
  invariant under the SlapdDatabase set, in both StatefulSets.
- The SlapdCluster controller's `Watches(&SlapdDatabase{})` keeps its purpose but
  changes its subject: it now maintains a ConfigMap, not a template.
- The reserved `_slaptain-auth` directory (ADR-027) is unaffected — it is
  unconditional in `bootstrap.sh` precisely because it must never ride in a
  list that changes.
- A database add becomes: Job per pod → wait → `ldap-add` per pod. The existing
  retry loop stays as the backstop it already is.
- Guard, red-first: an e2e asserting that applying a new `SlapdDatabase` to a
  Running cluster leaves the StatefulSet revision and every pod's UID untouched
  — the same assertion shape `scale_topology_test.go` already uses for scaling,
  and red against the current code by construction.

### Validated on a three-site mesh, 2026-09-28

Full suite at three sites (scm-s1/s2/s3, pod-routed), 96 passed / 1 failed /
931 s; the one failure was a nil-pointer panic in `scale_topology_test.go`'s
own setup — `spec.replication.network` is a pointer and nil is the ordinary
mesh shape — not a defect in this work. Fixed, then `scale-topology` and
`database-hotadd` re-run together: 4/4 green, `DataPresent=True` on every
site's databases afterwards.

Measured cost of an add: ~60 s from CR to Running, ~50 s for the remove. That
is the Job plus the 10 s requeues it rides, and it replaces a rolling restart
of every pod at every site.

Three defects were found by running it, none of which a unit test would have
produced, because all three are *absences* rather than failures:

1. **The Job ran twice per RW pod.** Two steps need the directories — the data
   DB and the accesslog DB — and both run in the same pass on a fresh database.
   Reaping from the observed terminal state (R1/R2) meant the second found the
   Job already gone and made an identical one. Memoised per pod per pass.
2. **A Job orphaned by a changed path set.** The name is a hash of (pod, paths),
   so the reap is keyed on the same hash: a path set that changes before the
   next pass — a second database, a scale across the 1↔2 accesslog boundary, an
   operator restart — leaves a Job nothing looks up again, and only the TTL
   removes it. Measured at 5m03s on a live cluster. Each pass now reaps this
   pod's Jobs from superseded path sets first, whatever their state.
3. Unrelated but found by the same second database: `slctl inspect` judged
   syncrepl on the FIRST data database only, including `rid-uniqueness`, whose
   entire purpose is a collision *between* databases.

## Amendment, 2026-10-07 (third): the count was not zero — two gates survived the audit

The second amendment retired the last *sanctioned* roll trigger and put the
count at zero. The count was two. Both were missed because the audit looked for
things that varied with the **database set**, which is what that amendment was
about, and these vary with the **replication posture**:

| Gate | What it put in the pod template |
|---|---|
| `NeedsAccesslogVolume()` — `replication.enabled && (replicas > 1 \|\| externalPeers > 0)` | the `/accesslog` volumeMount, on the init AND slapd containers |
| `spec.replication.enabled` | `LDAP_REPLICATION_ENABLED=true` on the init container |

Both flip on the same edit, and it is the commonest edit there is: a standalone
cluster becoming an HA one raises `spec.replicas` and sets `replication.enabled`
in one apply. The StatefulSet then creates the new pods at the new revision,
finds pod-0 stale, and replaces it.

### How it reads from outside

As a cluster that reaches `Running` and then goes `Degraded` for no stated
reason. Caught on a live demo, scaling `1 -> 3`:

```
Degraded 1/1 → Degraded 1/2 → Degraded 2/3 → Running 3/3 → Degraded 2/3 → Running 3/3
```

The `Running` **precedes** the `Degraded`, which is what makes it unreadable.
With `OrderedReady` the StatefulSet brings every new pod to Ready before it
touches the stale one, so the operator sees `readyReplicas == spec.replicas` and
reports `Running` while a pod replacement is still queued behind it.

The evidence that identifies it, none of which is where one looks first:

- `restartCount: 0` on every pod. It is a delete/recreate, not a restart, so the
  counter everyone reaches for is silent.
- **pod-0 is younger than pod-1 and pod-2.** Under `OrderedReady` pod-0 is
  created first and must be the oldest; an inverted age is the fingerprint.
- Two `ControllerRevision`s for the StatefulSet, the second created at the
  scale-up.

### Decision

Neither gate earns its keep. Both effects are carried unconditionally on every
RW pod:

- **the mount** — the accesslog *volume* was already unconditional, because
  `volumeClaimTemplates` is immutable and a later add would have needed a manual
  STS recreate with `--cascade=orphan`. The mount now follows the volume it has
  always had. A standalone pod gets empty `/accesslog/<db>` directories and no
  LMDB.
- **the env** — two extra `moduleload` lines (accesslog, syncprov) in the
  bootstrap `slapd.conf`. Modules can only be loaded at slapd startup, which is
  precisely why the gate was wrong: it read the condition at the one moment the
  answer was still "no". The runtime fallback (`ensureModulesLoaded`,
  `docs/reconcile-loop-fixes.md` 2026-07-15) stays for pods bootstrapped by an
  older operator.

This is the rule `olcServerID` has followed since ADR-017 — stamped on every RW
pod including a standalone one, because identity and capability are cheap to
carry and expensive to acquire later. The gates were an attempt to keep a
standalone cluster tidy, and they bought that tidiness with a pod replacement.

### Consequences

- `replicas: 1 → N` and `replication.enabled: false → true` stop rolling pods.
  The count of sanctioned pod-template roll triggers is zero, and this time it is
  asserted rather than asserted *about*.
- **One upgrade roll.** The RW pod template changes, so adopting this operator
  version rolls each cluster's RW pods once. That is the price of never rolling
  them on a scale-up again. The read-only template is byte-identical — the new
  env var is appended where it already sat on an RO pod — so RO pods do not roll.
- A standalone cluster carries a ~1Gi accesslog PVC (already true), an empty
  mount, and two loaded modules.
- `NeedsAccesslogVolume()` survives for the ephemeral Jobs (backup, restore,
  mkdir), where it answers a different question — does this database have a
  journal — and where rolling nothing is not at stake.

### Coverage

`statefulset_replica_churn_test.go` asserts the RW pod template is invariant
under both fields, across `1 <-> 2` as well as above it. That boundary was
previously **exempted in a comment**, on the grounds that `NeedsAccesslogVolume()`
"legitimately" changes the mounts there — the defect was written down as an
exemption and then stopped being looked at. Both tests were taken red first
against the old code, naming the offending mount and env var.

At the e2e tier, `scaleup_test.go` already performed this exact transition
(`replicas 1→2` + `replication.enabled` flip on a live cluster) and asserted
convergence afterwards — which stays green through a roll, since a replaced
pod-0 re-bootstraps with replication already on. It now snapshots pod-0's UID
and the StatefulSet revision across the transition.

## Amendment, 2026-10-08: a field the user flips to change the pod is not a trigger

The second amendment's rule reads "no spec field that can change during a cluster's
life may reach the pod template". Taken literally it already outlaws `spec.logLevel`,
whose own field comment says changing it rolls the pods, and it would outlaw
`spec.monitoring.enabled` (ADR-033's exporter sidecar). That is not what the rule is
for. Every trigger this ADR retired was a roll **nobody asked for**: a pod template
that changed because a database was added, because the replica count moved, or because
an operator upgrade derived something new.

**The rule, stated with its scope:** no spec field that changes *as a side effect* — of
another resource's lifecycle, of scaling, of state the operator derives, or of an
operator upgrade — may reach the pod template. A field whose purpose is to change the
pod — an image, `resources`, `logLevel`, `monitoring.enabled` — rolls when the user
changes it, and only then.

What makes such a field compliant, and what its tests must pin:

- **Unset or off is byte-identical to the template without the feature.** Upgrading the
  operator onto an existing cluster rolls nothing; the roll happens when the user opts in.
- **It defaults to the state that adds nothing to the pod.** An opt-out field would make
  the upgrade itself the trigger.

`spec.monitoring.enabled` is the first field added under this statement; `logLevel` was
compliant before it was written down.
---

## Amendment, 2026-10-15: this ADR does not win a conflict by default

ADR-035 states the rule for what happens when this decision collides with
another, and clarifies the scope of this one: its subject is the
**SlapdCluster's life** — a database added, a replica count moved, a value the
operator derived while the cluster runs. An **operator upgrade** is not one of
those. A new operator version may change the pod template once; it may still
never roll a fleet as a side effect of something a user did to another
resource, which is what this ADR was written about and what stands unchanged.

The first case decided this way: ADR-034 carries its startup-only table in a
ConfigMap named for what it holds and accepts the single roll at the upgrade
that introduces it, rather than hiding the table inside this ADR's own
`database-dirs` ConfigMap to avoid the restart.
