# Backlog (non-migration tech debt)

Cross-cutting cleanup items that aren't tied to the migration plan
(`docs/MIGRATION-PLAN.md`) or a single ADR. Keep entries small and actionable.

## Deprecation cleanup: `client.Apply` → `client.Client.Apply()` / `SubResource().Apply()`

**What:** controller-runtime deprecated the package-level `client.Apply` patch
type (staticcheck `SA1019`). All controllers currently use it for server-side
apply, e.g.:

```go
r.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner(...))
r.Status().Patch(ctx, p, client.Apply, client.ForceOwnership, client.FieldOwner(...))
```

`make -C operator lint` flags this in `slapdcluster_controller.go` (×6),
`slapddatabase_controller.go`, `slapdschema_controller.go`, and
`slapdbackup_controller.go` — a handful of `SA1019` hits.

**Why deferred:** it's a uniform, mechanical migration to the newer
`client.Client.Apply()` / `client.Client.SubResource("status").Apply()` API
across every controller. Doing it wholesale in one focused change is cleaner
(and easier to review) than touching it inline inside unrelated feature work.
We are not ignoring it — it's parked here deliberately.

**How:** migrate every `client.Apply` call site in one pass; re-run
`make -C operator lint` to confirm the `SA1019` count drops to zero. Verify the
field-manager ownership semantics are unchanged (same `FieldOwner`,
`ForceOwnership`).

**Note:** `make -C operator lint` currently reports broader pre-existing debt
too (errcheck, gofmt, modernize, unused, gocyclo, lll — ~70 findings as of
2026-06-08). Worth a separate sweep, but out of scope for this entry.


## e2e framework: specs cannot provision their own topology

**What:** the Go e2e suite cannot stand up the environment it runs against. The
primary fixture — namespace, operator install, TLS, NodePort services, node
access IPs, cross-site kubeconfigs, versitygw — is provisioned by
`tests/e2e.sh` before `go test` starts, and `BeforeSuite` simply assumes it is
there. Specs *can* create `SlapdCluster` CRs (`restore_test.go`,
`restore_inplace_test.go`, `restore_replication_test.go` each do), but there is
no framework for "give me a cluster with topology X, reachable, with
credentials": every such spec hand-rolls the same boilerplate — clone images and
pull secrets from the source cluster, mirror the suffix, expose a NodePort,
read the generated password, wait for readiness — and depends on `E2E_NODE_IP`
being exported by the shell.

**Why it matters:** any behaviour that only appears in a *particular topology*
is effectively untestable without either bending the shared fixture or
duplicating that boilerplate again. It is why the whole suite leans on one
shared, mutable `slapd` cluster that ~15 specs read from and three actively
damage, and why cross-spec interference is a recurring failure mode (see the
`FAIL_FAST` / `E2E_SEED` knobs added for exactly this reason).

**Why deferred:** it is a refactor of the test framework, not a change to the
product, and doing it inline inside feature work would bury it. It wants a
deliberate design pass: what a "cluster fixture" helper looks like, whether
`e2e.sh` shrinks to bootstrapping only the operator and the S3 target, and how
per-topology fixtures are torn down without leaking PVCs.

**How:** introduce a fixture helper that provisions a `SlapdCluster` +
`SlapdDatabase` of a requested shape (replica count, replication on/off,
external peers or none, TLS or not), exposes it, returns a connected client, and
registers its own cleanup. Port the three existing hand-rolled cases onto it
first — they are the specification for what the helper must do.

## Permanent e2e coverage for in-place restore topologies

**What:** `SlapdRestore` has three topologies with different meanings (ADR-014,
amendment 2026-08-24), and automated coverage exists for only one:

| Topology | Meaning | Coverage |
|---|---|---|
| `replicas: 1`, replication off | true rollback | `restore_inplace_test.go` — green 2026-08-24 |
| N≥2, no external peers | true rollback; the accesslog wipe matters here | **verified manually only** |
| mesh member (external peers) | local re-seed, mesh repairs | none — and none is wanted as a *rollback* assertion |

`restore_replay_test.go` is the spec for row 2 — it guards the accesslog-wipe fix
(`fix(restore): wipe the accesslog LMDB on restore under replication`). It
currently runs against the **shared** primary cluster, so on a multi-site
deployment it asserts rollback semantics in the one topology where they are not
promised, and fails deterministically. It needs its own N≥2 cluster with no
external peers.

**Why deferred:** blocked on the e2e framework item above — relocating it means
hand-rolling a fourth bespoke cluster, which is the duplication that item exists
to remove. Until then the spec's red on multi-site runs is *expected* and should
not be read as a product regression.

**How:** once the fixture helper exists, move the spec onto a dedicated N≥2,
no-external-peers cluster. That restores its validity, closes the row-2 gap, and
removes the most destructive spec from the shared fixture as a side effect.

## Flake: bootstrapFrom restore intermittently never sets restoreApplied

`restore [It] restores a backup into a fresh cluster via bootstrapFrom` has failed
**2 of 5 observed runs** (2026-08-25/26), always the same way: timeout after 480 s
waiting for `restoreApplied=true`.

What is established:

- `slapd-restore-0` refuses connections on 1024 for the entire window
  (`dial ...:1024: connect: connection refused`, logged by the SlapdDatabase
  controller every 10 s).
- Because the cluster therefore never reaches `Running`, the restore state machine
  **never enters preflight** — the SlapdCluster controller logs a single line for
  that cluster for the whole run, where a healthy run shows
  `entering restore (preflight)` → `preflight passed; scaling down`.
- So `restoreApplied` was never going to flip. The timeout is a symptom; the pod
  not starting is the fault.

What is **not** established: why slapd does not start. An initial theory —
orphaned PVCs from a previous run being rebound with populated `/config` and
`/data` — is **refuted**: a later session cleared the orphans before every run and
the failure still occurred once. The `slapd-ip` and `slapd-rr` restore clusters in
the same runs start fine.

Why it had resisted diagnosis: the spec's cleanup deletes the SlapdCluster and
SlapdDatabase, so by the time anyone looked there was no pod, no CR, and no pod log
left to inspect.

### 2026-09-12: the failure is now inspectable; the flake did not reproduce

Both halves of "make it inspectable" have landed in the restore specs
(`restore_test.go` hosts the helpers; `restore_inplace_test.go` and
`restore_replay_test.go` use them):

- **Autopsy at failure time** (`dumpRestoreAutopsy`, an `AfterEach` that fires only
  on a failed spec, before any teardown): SlapdCluster + SlapdDatabase status YAML,
  every pod's phase/conditions/per-container state, the `init` and `slapd` container
  logs (plus the *previous* container's log when it has restarted), and the
  namespace events for `<cluster>*`. It runs regardless of the keep setting, so even
  a run that must leave nothing behind prints its own post-mortem.
- **Keep-on-failure** (`keepOnFailure`, default **on**; `E2E_KEEP_ON_FAILURE=0`
  restores unconditional teardown): a failed restore spec leaves its cluster,
  database, backup, service and PVCs standing and prints what was kept, the
  `slctl debug-dump -n <ns> <cluster>` one-liner, and the cleanup commands.
- **PVC leak fixed spec-side** (`deleteRestorePVCs`): green runs now delete the
  `volumeClaimTemplates` PVCs by `app.kubernetes.io/instance`. Operator behaviour is
  untouched — ADR-005's Retain default stands.

Verified by mutation (the tooling's red): the `restoreApplied` timeout was
temporarily cut to 6 s so the spec failed exactly the way the flake does — with
`slapd-restore-0` not yet serving. The autopsy printed the full init-container log,
the pod's container states, 32 events, and the keep banner; the cluster was still
standing afterwards. Mutation reverted.

**Hunt: 16 consecutive green runs, no reproduction** (scm 3-site lab, build
`fd9b420`, OpenLDAP 2.7.1): 10 runs of the bootstrapFrom spec alone
(`E2E_LABEL_FILTER=restore-bootstrap`, ~40 s each) and 6 runs of the whole
`restore` label group (~172 s each, 4 specs, replay skipped as expected on a mesh).
Every run left zero `slapd-restore*` / `slapd-ip*` / `slapd-rr*` objects behind.
So: unreproduced on *this* substrate, which is fast enough that the whole spec
finishes in 37 s against a 480 s timeout — i.e. the failing runs were roughly an
order of magnitude off normal, not marginally slow.

Still **not established**: why slapd does not start. Candidate mechanisms to check
the next time it fires, now that the evidence survives — in the order the autopsy
answers them:

1. Pod never scheduled or stuck mounting (events: `FailedScheduling`,
   `FailedAttachVolume`, `FailedMount`). The lab's storage is node-local RWO
   (`openebs-lvm`), and ADR-018's deletion-lease rule means any pod object still
   naming one of the three PVCs pins the volume to a node.
2. Init container looping or exiting non-zero (init log + `state=terminated(exit=…)`).
3. slapd crash-looping (`restarts>0` plus the *previous* container log — the reason
   a slapd that dies at startup leaves an empty current log).
4. Pod healthy but unreachable, i.e. a service/DNS/routing fault rather than a slapd
   fault (pod `Ready=True` while the SlapdDatabase reports `NoPodsReachable`).

Note that (4) is the normal *transient* state for the first ~10 s of every run — the
SlapdDatabase legitimately passes through `phase=Error`/`NoPodsReachable` before the
pod serves. The flake is that state persisting, not its appearance.

---

## Cross-site recovery after a pod-IP change: the bottleneck is downstream of discovery

Supersedes the earlier item "Peer address discovery has no vote in the requeue
decision", whose premise **measurement refuted**.

Under ADR-016 pod-routed transport, replacing a pod invalidates every peer's syncrepl
addresses and recovery takes minutes. The theory was that peer address discovery rides
on the 60 s `csnCheckInterval` and cannot ask for anything faster. That was
implemented — a 15 s discovery cadence decoupled from CSN monitoring, plus a 3 s
settle-tightening while the observed address set is still changing — and measured on
three pod-routed sites:

| build | full site restart | single pod |
|---|---|---|
| baseline | 146 s, 353 s, 308 s | 31 s, 6 s |
| decoupled cadence | 189 s, 360 s, 145 s | 75 s, 34 s |

Indistinguishable, with the mechanism verifiably firing (15 s reconcile spacing,
discovery-only passes logged). Reverted (`e692190`); implementation preserved in
`11dc2c4` if it is ever wanted as a component of a real fix.

A decomposition trace of one full-restart recovery:

    t+7s    siteA Ready with new pod IPs
    t+148s  siteB's status carries all the new IPs
    t+289s  a siteA write is visible on siteB
    t+432s  siteB's syncrepl stanzas name the new IPs

**The dominant term is downstream of discovery and is still unidentified** —
stanza-rewrite scheduling, or slapd's own consumer reconnect. The trace is also
internally inconsistent (stanzas appearing to be rewritten *after* the write was
already replicating), and the probe that produced the stanza timing was unreliable:
siteB names one provider per peer, so an "all IPs present" check can never pass.

**Next step for whoever takes this: instrument the stanza-rewrite path, do not tune
another interval.** Specifically, timestamp (a) the `SlapdCluster` status write
carrying new addresses, (b) the `SlapdDatabase` reconcile that consumes it, (c) the
`olcSyncRepl` modify landing on each pod, and (d) the consumer's first successful
connection. Note the baseline spans 146-353 s, so any claim of improvement needs
distributions rather than single samples.

Worth checking early, because it would explain the gap: the `SlapdDatabase`
controller's requeue is 10 s only on *unmet preconditions*, and a pod whose database
reconcile fails is skipped for stanza writes entirely (the ADR-019 R8 stanza-deferral
fix). During a mass pod replacement, pods legitimately fail that reconcile for a
while — so stanza rewrites may be deferred for reasons unrelated to discovery.

---

## SlapdDatabase and SlapdSchema need an explicit periodic resync

Prerequisite for any watch filtering, and a gap in its own right. See the ADR-002
amendment and `docs/reconcile-loop-fixes.md`, both 2026-08-26.

Neither controller requeues on its success path, so neither re-examines per-pod
`cn=config` on any schedule. The resync that exists today is **incidental**:
`checkPeerCSNConvergence` stamps `lastChecked` every 60 s, that changes the
`SlapdCluster` status, and the unfiltered watch re-reconciles every database in the
namespace. Everything that converges without a CR change rides on it — drift, hand
edits, the ADR-019 R8 migration, re-applied ACLs.

Two things follow:

- **Filtering that watch is unsafe until the resync is explicit.** Already attempted
  and reverted: a predicate faithful to the four fields the controller reads failed
  four e2e specs.
- **A cluster with `replication.enabled: false` has no periodic resync at all**, since
  the `SlapdCluster` controller only requeues periodically when replication is
  enabled. Standalone-cluster drift is corrected only if some unrelated event fires.
  Nobody intended this and no e2e covers it.

The work: give both controllers their own `RequeueAfter` sized for drift correction
(independent of `csnCheckInterval`, which exists for CSN monitoring), add an e2e that
proves drift is corrected without any CR change — hand-edit an ACL on one pod, wait,
assert it is restored — and only then revisit the watch predicate. That e2e is the
piece with real value: it would have caught the reverted change directly rather than
via four unrelated failures.

---

## Cross-site replication health is not readable from one site

Not a defect — a documentation and tooling gap, recorded so nobody builds
monitoring on a false assumption. See the ADR-008 amendment of 2026-08-26.

`status.externalPeerStatuses` is **consumer-side**: it describes this site's
*inbound* links. Because delta-syncrepl is pull-based and a provider keeps no
consumer registry, a provider cannot observe that a consumer stopped consuming from
it. So if B stops consuming from A, that is visible on **B**, and A correctly reports
`Synced` at the same time. Reading mesh health means reading every site's CR.

Two follow-ups worth considering:

- **`slctl` has no cross-site view.** `slctl inspect` is single-cluster. Something
  that takes several contexts and joins each site's peer statuses into one mesh
  verdict would turn "read four CRs and correlate by hand" into one command. This is
  the natural home for the join, and it needs no operator change.
- **Lag is only observable under traffic.** `csnSyncThreshold` is 5 s, so a broken
  link shows as `Lagging` promptly *while writes flow*; on an idle database both
  sides sit at the same CSN and the state reads `Synced` across an arbitrarily broken
  link. Heartbeat writes would close that, and per the ADR-008 amendment of
  2026-09-11 would also close a real gap: idleness lets a serverID go dormant,
  which is what makes `syncprov`'s minCSN fallback pick a stale lookup key and
  trigger the ITS#9580 "sync cookie is stale" full-refresh storm on reconnect
  (see `docs/INVESTIGATION-replication-divergence-after-dataloss-and-restart.md`).
  They stay deferred anyway: the OpenLDAP 2.7 upgrade (ADR-021) and the syncprov
  sessionlog's per-SID viability check (ADR-022) act on that defect directly, and
  the objection that the operator would be writing into the user's data tree
  still applies to a heartbeat, which would only mask it. This is the second
  time the system has turned out to depend on periodic activity nobody
  deliberately generates — the first was the incidental 60 s-tick resync
  found in the ADR-002 amendment (2026-08-26).

---

## e2e cannot pin an old `slapd-init` image, so one migration failure mode has no guard

The 2026-08-25 "syncrepl stanzas written to a pod whose accesslog DB does not
exist" fix is proven only by hand-run live scenarios. A permanent regression test
needs pods whose init container predates the per-database accesslog directory
creation, because the missing thing is a *directory on a PVC the operator cannot
touch* (ADR-018) — it is not manufacturable from `cn=config` the way the legacy
shared log is (`E2E_ACCESSLOG_MIGRATION=1` does exactly that trick).

What is missing is a fixture capability: deploying a cluster with `spec.images`
pinned to a chosen older init tag, then upgrading the operator underneath it.
That overlaps with "e2e framework: specs cannot provision their own topology"
above and should probably be solved with it rather than separately.

---

## Cross-site orchestration (hub-and-spoke) — explicitly undecided

**What:** automating the mesh-wide rollback runbook (`docs/BACKUP.md`, "Rolling
back a replicated deployment") so it becomes one declarative action instead of a
careful manual sequence across N clusters.

**Why this is not just a feature:** it requires one actor to drive *other*
clusters — quiesce them, run restores there, and sequence the result. Today the
operator reaches across clusters only to **read**: it discovers peer pod
addresses through a remote kubeconfig (ADR-007 amendment, ADR-016). Driving a
remote site is categorically different. It introduces a control plane, and with
it hub failure, partition behaviour, and arbitration — and it stands in direct
tension with this project's second architectural requirement, that each site be
autonomous.

**Why deferred, and deliberately undecided:** this is a fundamentals question,
not a backlog chore. It is recorded here so the option is not lost, *not* as an
agreed direction. The current answer — a human-operated distributed runbook — is
legitimate and is how comparable systems document the same operation. Nothing
about it is broken; it is manual.

**If pursued:** it gets its own ADR, deciding the topology (a control-plane
cluster? a peer-elected coordinator? a CLI-driven sequence with no new
controller?) and the failure semantics before any code. See the ADR-014
amendment (2026-08-24), section "Not decided: hub-and-spoke orchestration".

---

## ADR-022 follow-ups: syncprov tuning convergence is inconsistent

`olcSpSessionlog` now converges on every reconcile (set / replace / delete —
ADR-022), but `olcSpCheckpoint` is still written only when the syncprov overlay
is first added: a later `syncprovCheckpoint` spec change is silently ignored on
pods whose overlay already exists. Align the checkpoint with the sessionlog's
converge-always pattern (the pure `planSessionlog` seam generalizes).

Separately: `syncprov-sessionlog-source` (the persistent, accesslog-backed
sessionlog) was rejected in ADR-022 because it reads the change journal — the
artifact that is contaminated or purged in exactly the scenarios where a
persistent log would pay off. Revisit once the journal's trustworthiness across
refresh/purge transitions is settled (upstream ITS#9580 work, ADR-021).

---

## ridBase cross-CR uniqueness is documented but not enforced

`spec.replication.ridBase` must be unique across the SlapdDatabase CRs of one
cluster (colliding values produce colliding RIDs, which corrupts per-consumer
cookie state). The field's doc comment used to claim "the operator validates
this" — it never did; the only admission rule is the CEL presence check. Found
2026-09-12 while retiring the foreignRIDs entry.

The fix is a reconcile-time check in the SlapdDatabase controller (list sibling
SlapdDatabases of the same cluster, refuse with phase=Error on a ridBase whose
stanza range overlaps another's), plus a red-first e2e: two databases with the
same ridBase, assert the second reports the collision instead of writing
colliding stanzas. A webhook would also work but the project has none — do not
grow one just for this.

---

## A version-crossing image change on a populated cluster fails without explanation

Editing a populated 2.6 cluster's `spec.images` to the 2.7 pair (or back)
crashloops every pod on volumes the new slapd cannot open (LMDB 1.0 format
break, ADR-021). The operator neither refuses the change nor explains the
failure — the user gets CrashLoopBackOff and has to find the runbook.

Honest constraints: image tags are free-form (nothing reliable marks a tag as
"2.6" or "2.7"), and ADR-018 means the operator cannot inspect the volumes. So
a hard admission guard is likely impossible without new API surface (e.g. an
explicit `spec.images.generation` the deployer asserts). The realistic minimum
is observability: when pods crashloop after an image change, surface the slapd
startup error (the LMDB version complaint is in the container log) into a
SlapdCluster condition with a pointer to docs/OPENLDAP-VERSIONS.md. Decide the
shape before building; do not grow a webhook for this.

---

## `kubectl rollout status` returns before readiness on slaptain StatefulSets

Observed during the v0.1.0 demo rehearsal: `kubectl rollout status
statefulset/slapd` completes in ~0.1 s with "partitioned roll out complete"
while the pod is still ContainerCreating — the documented wait gates were
decorative. The controller sets no `updateStrategy` explicitly, so the
partition involved is API-server defaulting; whether kubectl's partitioned
branch (which checks `updatedReplicas`, never readiness) should trigger on a
defaulted partition of 0 needs root-causing before blaming anyone. Fix
Discipline applies: reproduce, read the kubectl branch condition, then decide
whether the operator should set an explicit strategy or the docs should simply
never use `rollout status` as a readiness gate (the demo now waits on
`status.readyReplicas`, which cannot lie).

---

## Designed, deferred: opt-in `SlapdBackup.spec.requireConverged` with a bounded wait

A backup always `slapcat`s pod-0, so on a replicating cluster that is briefly
behind, a write ACKed elsewhere can legitimately be missing from the artifact
(demonstrated 2026-09-12; ADR-014 amendment of that date). That amendment fixed
the *silence*: every backup now records `status.sourcePod`,
`status.sourceContextCSN` and a `SourceConverged` condition. It deliberately did
**not** add a gate.

The opt-in gate is designed and parked here until someone asks for it:

- `spec.requireConverged: true` (never default) plus `spec.convergenceTimeout`;
- while the target `SlapdCluster` reports `ReplicationConverged != True`, the
  backup stays `Pending` with a reason saying what it is waiting for;
- on timeout it **takes the backup anyway** and records that it waited and gave
  up. A backup that declines to run is a backup you do not have; "requireConverged"
  must never become "no artifact exists".

Rejected outright, and not to be revisited without new information: a strict
CSN-dominance gate (refuse unless the source's CSN vector dominates every
peer's). A replica of a heavily-written provider never dominates, and under
multi-master two vectors can be mutually incomparable — the predicate is both
unusable and ill-defined.

Also unbuilt, cheap, and independent: stamping `sourcePod` + the converged flag
into the S3 **object metadata** at upload, so a restore can surface them without
the CR. `backup.Upload` already accepts a metadata map; what is missing is a way
for the Job's uploader to carry arbitrary pairs (`manager backup-upload
--meta k=v`).

---

## Scale and operations tunables — from the production-config review, 2026-09-12

A review of slaptain's generated `cn=config` against a large production OpenLDAP
platform produced 18 findings. The five that break a large cluster outright are
have **landed** (2026-09-12); **ADR-024 fixes where each of these lives**
(converged per pod / bootstrap-time / create-only-by-nature, and which CR owns
it), so the entries below record the gap and the severity, not the design. Pick
any of them up by reading ADR-024's placement table first — the class is already
decided.

Already done, listed so nobody re-raises them: the replication identity's
`olcLimits` exemption on the data and accesslog databases (ADR-020 amendment —
this was a silent 500-entry replication cap), `olcDbMaxSize` on both with
operator defaults and a converge-on-change path that refuses a shrink out loud,
per-database `sizeLimit`/`timeLimit`/`limits`, the `entryCSN`/`entryUUID`
baseline indices on data databases, and `spec.backend.idlExponent`. The
many-entries fixture class ADR-024 asks for is `tests/e2e/scale_test.go`, gated
`E2E_SCALE=1` — every one of the five is structurally invisible without it.

**Landed 2026-09-13** (the degrades-and-hygiene half): `olcDbCheckpoint`,
`olcDbNoSync` (now converged and inheriting a cluster-wide
`spec.tuning.noSync`), `olcDbRtxnSize`, `olcDbEnvFlags` (create-only and
reported), the syncrepl stanza timeouts and a default keepalive,
`slapadd -q` + `spec.tuning.toolThreads` for restores,
`spec.ldap.tls.{protocolMin,cipherSuite}`, `spec.ldap.passwordHash`, and a
`logLevel` default of 16640. Guarded by `tests/e2e/tunables_test.go` (ungated).
Three of the review's items did NOT survive contact with a live cluster and are
re-entered below with their evidence.

- **Connection lifetimes cannot be converged — `olcIdleTimeout` and
  `olcWriteTimeout`** (degrades; was the urgent half of the review's finding
  10). slapd's default for both is *never close*, so a pod behind a stateful
  firewall accumulates connections whose peer is long gone until it runs out of
  file descriptors. The gap is real. The **placement was wrong**: an
  `ldapmodify` of either attribute against a running OpenLDAP 2.7.1 does not
  fail and does not crash — **it hangs the server**. The modify's CSN is queued
  and never graduates, the pod then answers nothing at all (not even an
  anonymous rootDSE), and `SIGTERM` sticks in `slapd shutdown: waiting for 2
  operations/tasks to finish`:

  ```
  conn=1050 op=2 MOD dn="cn=config"
  conn=1050 op=2 MOD attr=olcIdleTimeout
  slap_get_csn: conn=1050 op=2 generated new csn=…
  slap_queue_csn: queueing …
  <nothing, ever>
  ```

  Reproduced twice on a healthy three-pod cluster (three of four pods the first
  time, all three RW pods the second); recovery was restarting every pod. The
  mechanism is the daemon thread: a non-zero `global_idletimeout` arms
  `connections_timeout_idle`, which walks the connection table from the daemon
  loop while the modify that armed it is still executing inside one of those
  connections. Both values are **fine when they come from the boot config** — a
  pod that *started* with `olcIdleTimeout 3600` / `olcWriteTimeout 300` served
  normally for twenty minutes. *Surface:* ADR-024 **R2**, bootstrap-time —
  `idletimeout`/`writetimeout` directives in the init container's generated
  `slapd.conf`, with R2's documented recreate path. No CRD field until then: a
  field the operator cannot honour is what R4 forbids.
- **Thread and buffer counts** (degrades, unmeasured). `olcThreads`,
  `olcListenerThreads`, `olcConcurrency`, `olcSockbufMaxIncoming`,
  `olcSockbufMaxIncomingAuth`, `olcConnMaxPending` are neither set nor exposed.
  Upstream's defaults are defensible and the reference production platform
  leaves them alone too, so these wait for a measurement that asks for one.
  `spec.tuning` is the home when one does.
- **No `cn=monitor`, no native metrics** (hygiene; *attempted 2026-09-13 and
  withdrawn*). The monitor backend module is not loaded, no monitor database
  exists, `olcMonitoring` is never set — so there are no per-database operation
  counters and nothing an exporter can read. Monitoring today is the operator's
  CSN polling, which by ADR-008's own amendment cannot see an idle-but-broken
  link.

  An implementation landed and was reverted the same day. Individually every
  piece works on a running 2.7.1 pod: `olcModuleLoad: back_monitor` is accepted,
  `olcDatabase=monitor` can be added live, `cn=Monitor` answers immediately, and
  an ACL granting read to the existing `cn=replication,<suffix>` identity
  (ADR-008 reuse) enforces correctly — anonymous gets `No such object`. What
  does **not** work is converging it: with a monitor database present, an
  `ldapmodify` of its `olcAccess` hangs slapd with the same signature as the
  connection timeouts above (CSN queued, never graduates, pod dead to all
  clients), observed on three of four pods. The operator rewrote that ACL every
  reconcile because its comparison of what it wrote against what slapd stores
  never matched — so steady state was one modify away from bricking the cluster.

  *Surface when picked up again:* fix the ACL comparison FIRST and prove the
  operator reaches a no-write steady state, then establish whether an `olcAccess`
  modify on a monitor database is safe at all. Until both are answered this is
  not a hygiene feature, it is an outage generator. The e2e assertions (monitor
  database present per pod; readable by the replication identity; denied
  anonymously) are worth restoring from commit `07b89af`'s parent.
- **TLS: no CRL policy, no `olcLocalSSF`/`olcSecurity`** (hygiene; the protocol
  floor and cipher policy landed 2026-09-13 as `spec.ldap.tls.protocolMin` /
  `.cipherSuite`). `olcTLSCRLCheck` and a minimum-SSF statement are still
  unstated, so a revoked peer certificate is accepted and there is nothing
  forcing a client onto TLS. *Surface:* the same converged block.
- **A stronger password-hashing scheme is not available** (hygiene; the policy
  field landed 2026-09-13 as `spec.ldap.passwordHash`, pinned to `{SSHA}` and
  converged). The module-availability question the review asked is answered:
  slaptain's runtime image does **not** ship `pw-argon2`, so `{ARGON2}` cannot be
  selected today — asking for it would produce a slapd that rejects every
  password write. Nor is `olcPasswordCryptSaltFormat` exposed. *Surface:* build
  the module into `images/slapd/Containerfile` first; the CRD field is already
  there to point at it.
- **Overlay surface beyond replication** (hygiene, document-only). We manage
  `syncprov` and `accesslog` and nothing else; the production platform also runs
  password-policy, uniqueness, dynamic-list and member-of overlays plus
  last-bind tracking. Not a scale defect — it is the difference between a
  replicating store and a directory an application stack can sit on. Worth a
  stated position (explicitly out of scope, or an ADR for a generic overlay
  surface on `SlapdDatabase`) rather than silence.

**Stale prior comparison.** An older written-down comparison lists
`olcSpCheckpoint`, `olcSpSessionlog`, `olcAccessLogPurge`,
`olcAccessLogOps`/`Success`, syncrepl `retry`, the accesslog indices and
per-database accesslog separation as missing. They are not: ADR-019, ADR-022 and
the syncprov overlay code cover all of them. Do not re-raise them from that
document.
